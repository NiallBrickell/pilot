package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/NiallBrickell/pilot/internal/approve"
	"github.com/NiallBrickell/pilot/internal/config"
	"github.com/NiallBrickell/pilot/internal/state"


	"github.com/google/uuid"
)

// Server is a lightweight HTTP server providing SSE events and grace-period control.
type Server struct {
	broker       *Broker
	pending      *PendingStore
	port         int
	srv          *http.Server
	evalSem      chan struct{} // semaphore to limit concurrent haiku evaluations
	toolCounts   sync.Map     // session_id → *toolCounter
	evaluatorURL string
	evalTimeout  time.Duration
	interrogationConfidence float64
}

// toolCounter tracks tool calls per session for checkpoint logic.
type toolCounter struct {
	mu              sync.Mutex
	countSinceUser  int
	lastUserMsgHash string // detect new user messages
}

// shouldInterrogate returns true if this tool call should include a
// context-aware checkpoint (is Claude still on track?).
// Fires on: 1st, 5th, then every 25th tool call after each user message.
func (tc *toolCounter) shouldInterrogate(userMsgHash string) bool {
	tc.mu.Lock()
	defer tc.mu.Unlock()

	if userMsgHash != tc.lastUserMsgHash {
		tc.countSinceUser = 0
		tc.lastUserMsgHash = userMsgHash
	}
	tc.countSinceUser++

	n := tc.countSinceUser
	return n == 1 || n == 5 || (n > 5 && (n-5)%25 == 0)
}

func New(cfg *config.PilotConfig) *Server {
	port := cfg.General.SSEPort
	if port == 0 {
		port = 9721
	}
	maxConcurrent := cfg.General.MaxConcurrentEvals
	if maxConcurrent <= 0 {
		maxConcurrent = 4
	}
	evalTimeout := time.Duration(cfg.General.EvaluatorTimeoutMs) * time.Millisecond
	if evalTimeout <= 0 {
		evalTimeout = 30 * time.Second
	}
	interrogationConf := cfg.General.InterrogationConfidence
	if interrogationConf <= 0 {
		interrogationConf = 0.7
	}

	broker := NewBroker()

	// Register webhooks from config
	for _, wh := range cfg.Webhooks {
		broker.AddWebhook(wh)
	}

	return &Server{
		broker:                  broker,
		pending:                 NewPendingStore(),
		port:                    port,
		evalSem:                 make(chan struct{}, maxConcurrent),
		evaluatorURL:            config.EvaluatorURL(cfg),
		evalTimeout:             evalTimeout,
		interrogationConfidence: interrogationConf,
	}
}

func (s *Server) Broker() *Broker {
	return s.broker
}

func (s *Server) Start() error {
	mux := http.NewServeMux()
	mux.HandleFunc("/events", s.handleSSE)
	mux.HandleFunc("/status", s.handleStatus)
	mux.HandleFunc("/internal/pending", s.handleInternalPending)
	mux.HandleFunc("/internal/action", s.handleInternalAction)
	mux.HandleFunc("/approve/", s.handleApprove)
	mux.HandleFunc("/reject/", s.handleReject)
	mux.HandleFunc("/internal/evaluate", s.handleEvaluate)
	mux.HandleFunc("/internal/evaluate-idle", s.handleEvaluateIdle)
	mux.HandleFunc("/hooks/install", s.handleHooksInstall)
	mux.HandleFunc("/hooks/uninstall", s.handleHooksUninstall)
	mux.HandleFunc("/config", s.handleGetConfig)
	mux.HandleFunc("/logs", s.handleLogs)

	s.srv = &http.Server{
		Addr:    fmt.Sprintf(":%d", s.port),
		Handler: corsMiddleware(mux),
	}

	slog.Info("SSE server starting", "port", s.port)
	if err := s.srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}

func (s *Server) Shutdown(ctx context.Context) error {
	if s.srv != nil {
		return s.srv.Shutdown(ctx)
	}
	return nil
}

// handleSSE streams events to connected clients.
func (s *Server) handleSSE(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	ch := s.broker.Subscribe()
	defer s.broker.Unsubscribe(ch)

	// Send initial connected event
	fmt.Fprintf(w, "event: connected\ndata: {\"port\":%d}\n\n", s.port)
	flusher.Flush()

	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case event, ok := <-ch:
			if !ok {
				return
			}
			if event.ID != "" {
				fmt.Fprintf(w, "id: %s\n", event.ID)
			}
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event.Type, event.Data)
			flusher.Flush()
		}
	}
}

// handleStatus returns current pilot state + hooks status as JSON.
func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	ps, err := state.ReadState()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// Check if hooks are installed
	home, _ := os.UserHomeDir()
	settingsData, _ := os.ReadFile(fmt.Sprintf("%s/.claude/settings.json", home))
	hooksInstalled := strings.Contains(string(settingsData), "pilot approve")

	// Merge into response
	raw, _ := json.Marshal(ps)
	var result map[string]any
	json.Unmarshal(raw, &result)
	result["hooks_installed"] = hooksInstalled

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(result)
}

// handleInternalPending is called by `pilot approve` to register a pending approval
// and block for the grace period.
func (s *Server) handleInternalPending(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		ToolName     string  `json:"tool_name"`
		ToolInput    string  `json:"tool_input"`
		Reason       string  `json:"reason"`
		Source       string  `json:"source"`
		Confidence   float64 `json:"confidence"`
		GracePeriodS float64 `json:"grace_period_s"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	now := time.Now()
	graceDuration := time.Duration(req.GracePeriodS * float64(time.Second))
	id := uuid.New().String()[:8]

	pending := &PendingApproval{
		ID:         id,
		ToolName:   req.ToolName,
		ToolInput:  req.ToolInput,
		Reason:     req.Reason,
		Confidence: req.Confidence,
		CreatedAt:  now,
		ExpiresAt:  now.Add(graceDuration),
		ResultCh:   make(chan string, 1),
	}
	s.pending.Add(pending)

	// Broadcast pending_approval event
	eventData, _ := json.Marshal(map[string]any{
		"id":             id,
		"tool_name":      req.ToolName,
		"tool_input":     req.ToolInput,
		"reason":         req.Reason,
		"source":         req.Source,
		"confidence":     req.Confidence,
		"expires_at":     pending.ExpiresAt.Format(time.RFC3339Nano),
		"grace_period_s": req.GracePeriodS,
	})
	s.broker.Publish(SSEEvent{
		ID:   id,
		Type: "pending_approval",
		Data: string(eventData),
	})

	// Block for grace period, waiting for human override
	var outcome string
	var resolvedBy string
	timer := time.NewTimer(graceDuration)
	defer timer.Stop()

	select {
	case decision := <-pending.ResultCh:
		outcome = decision
		resolvedBy = "human"
	case <-timer.C:
		outcome = "approved"
		resolvedBy = "timeout"
		s.pending.Remove(id)
	}

	// Broadcast approval_resolved event
	resolvedData, _ := json.Marshal(map[string]any{
		"id":          id,
		"outcome":     outcome,
		"resolved_by": resolvedBy,
	})
	s.broker.Publish(SSEEvent{
		ID:   id,
		Type: "approval_resolved",
		Data: string(resolvedData),
	})

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{
		"outcome":     outcome,
		"resolved_by": resolvedBy,
	})
}

// handleInternalAction is called by pilot commands to emit action events.
func (s *Server) handleInternalAction(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		Timestamp  string   `json:"timestamp"`
		ActionType string   `json:"action_type"`
		Detail     string   `json:"detail"`
		Confidence *float64 `json:"confidence"`
		ToolName   string   `json:"tool_name,omitempty"`
		ToolInput  string   `json:"tool_input,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	eventData, _ := json.Marshal(req)
	s.broker.Publish(SSEEvent{
		ID:   uuid.New().String()[:8],
		Type: "action",
		Data: string(eventData),
	})

	w.WriteHeader(http.StatusOK)
}

// handleApprove resolves a pending grace-period approval as "approved".
func (s *Server) handleApprove(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	id := strings.TrimPrefix(r.URL.Path, "/approve/")
	if !s.pending.Resolve(id, "approved") {
		http.Error(w, "not found or already resolved", http.StatusNotFound)
		return
	}
	w.WriteHeader(http.StatusOK)
}

// handleReject resolves a pending grace-period approval as "rejected".
func (s *Server) handleReject(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	id := strings.TrimPrefix(r.URL.Path, "/reject/")
	if !s.pending.Resolve(id, "rejected") {
		http.Error(w, "not found or already resolved", http.StatusNotFound)
		return
	}
	w.WriteHeader(http.StatusOK)
}

// handleEvaluate runs approval evaluation via the Node evaluator sidecar.
func (s *Server) handleEvaluate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		ToolName       string `json:"tool_name"`
		ToolInput      string `json:"tool_input"`
		Cwd            string `json:"cwd"`
		SessionID      string `json:"session_id"`
		TranscriptPath string `json:"transcript_path"`
		UserMsgHash    string `json:"user_msg_hash"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	// Check if this is an interrogation point
	if req.SessionID != "" && req.TranscriptPath != "" {
		raw, _ := s.toolCounts.LoadOrStore(req.SessionID, &toolCounter{})
		tc := raw.(*toolCounter)
		if tc.shouldInterrogate(req.UserMsgHash) {
			if redirect := s.interrogate(req.TranscriptPath, req.ToolName, req.ToolInput, req.Cwd); redirect != "" {
				w.Header().Set("Content-Type", "application/json")
				json.NewEncoder(w).Encode(map[string]any{
					"decision":   "deny",
					"reason":     redirect,
					"source":     "interrogate",
					"tool_name":  req.ToolName,
					"cwd":        req.Cwd,
					"session_id": req.SessionID,
				})
				return
			}
		}
	}

	// Layer 1 + 2: Check Claude settings and pilot rules before hitting LLM
	cfg := config.Load()
	if decision := approve.Evaluate(cfg, req.ToolName, req.ToolInput, req.Cwd); decision != nil {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"decision":   decision.Action,
			"reason":     decision.Reason,
			"source":     decision.Source,
			"tool_name":  req.ToolName,
			"cwd":        req.Cwd,
			"session_id": req.SessionID,
		})
		return
	}

	// Layer 3: Call Node evaluator sidecar (haiku)
	evalBody, _ := json.Marshal(map[string]string{
		"system_prompt": cfg.Prompts.Approval,
		"tool_name":     req.ToolName,
		"tool_input":    req.ToolInput,
	})

	client := &http.Client{Timeout: s.evalTimeout}
	evalResp, err := client.Post(s.evaluatorURL+"/evaluate-approval", "application/json", bytes.NewReader(evalBody))
	if err != nil {
		slog.Warn("Evaluator sidecar not reachable", "error", err)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"decision":   "deny",
			"reason":     fmt.Sprintf("evaluator error: %v", err),
			"tool_name":  req.ToolName,
			"cwd":        req.Cwd,
			"session_id": req.SessionID,
		})
		return
	}
	defer evalResp.Body.Close()

	var evalResult struct {
		Decision string `json:"decision"`
		Reason   string `json:"reason"`
	}
	json.NewDecoder(evalResp.Body).Decode(&evalResult)

	// Emit SSE event
	now := time.Now().UTC()
	actionType := "escalate"
	confidence := 0.0
	if evalResult.Decision == "approve" {
		actionType = "auto_approve"
		confidence = 1.0
	}
	eventData, _ := json.Marshal(map[string]any{
		"timestamp":   now.Format(time.RFC3339Nano),
		"action_type": actionType,
		"detail":      fmt.Sprintf("%s: %s", req.ToolName, evalResult.Reason),
		"confidence":  confidence,
		"tool_name":   req.ToolName,
		"tool_input":  req.ToolInput,
		"cwd":         req.Cwd,
		"session_id":  req.SessionID,
	})
	s.broker.Publish(SSEEvent{
		ID:   uuid.New().String()[:8],
		Type: "action",
		Data: string(eventData),
	})

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"decision":   evalResult.Decision,
		"reason":     evalResult.Reason,
		"confidence": confidence,
		"tool_name":  req.ToolName,
		"cwd":        req.Cwd,
		"session_id": req.SessionID,
	})
}

// handleEvaluateIdle runs idle evaluation via the Node evaluator sidecar.
func (s *Server) handleEvaluateIdle(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		TranscriptContext string `json:"transcript_context"`
		Cwd               string `json:"cwd"`
		SessionID         string `json:"session_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	cfg := config.Load()
	evalBody, _ := json.Marshal(map[string]string{
		"system_prompt":      cfg.Prompts.AutoRespond,
		"transcript_context": req.TranscriptContext,
	})

	client := &http.Client{Timeout: s.evalTimeout}
	evalResp, err := client.Post(s.evaluatorURL+"/evaluate-idle", "application/json", bytes.NewReader(evalBody))
	if err != nil {
		slog.Warn("Evaluator sidecar not reachable for idle", "error", err)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"should_respond": false,
			"message":        "",
			"confidence":     0,
			"reasoning":      fmt.Sprintf("evaluator error: %v", err),
		})
		return
	}
	defer evalResp.Body.Close()

	var evalResult struct {
		ShouldRespond bool    `json:"should_respond"`
		Message       string  `json:"message"`
		Confidence    float64 `json:"confidence"`
		Reasoning     string  `json:"reasoning"`
	}
	json.NewDecoder(evalResp.Body).Decode(&evalResult)

	// Emit SSE event
	now := time.Now().UTC()
	actionType := "auto_respond_skipped"
	if evalResult.ShouldRespond {
		actionType = "auto_respond"
	}
	eventData, _ := json.Marshal(map[string]any{
		"timestamp":   now.Format(time.RFC3339Nano),
		"action_type": actionType,
		"detail":      evalResult.Message,
		"confidence":  evalResult.Confidence,
		"cwd":         req.Cwd,
		"session_id":  req.SessionID,
	})
	s.broker.Publish(SSEEvent{
		ID:   uuid.New().String()[:8],
		Type: "action",
		Data: string(eventData),
	})

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"should_respond": evalResult.ShouldRespond,
		"message":        evalResult.Message,
		"confidence":     evalResult.Confidence,
		"reasoning":      evalResult.Reasoning,
	})
}

// interrogate reads the conversation context and checks if Claude is still
// on track. Returns a redirect message if off-track, empty string if fine.
func (s *Server) interrogate(transcriptPath, toolName, toolInput, cwd string) string {
	// Build conversation summary (reuse the same logic as on-stop)
	summary := buildTranscriptSummary(transcriptPath)
	if summary == "" {
		return "" // Can't interrogate without context
	}

	evalBody, _ := json.Marshal(map[string]string{
		"system_prompt": `You are monitoring an AI coding assistant (Claude Code) to check it's still on track.

You'll see: the user's original request, their RECENT messages, and the tool call Claude is about to make.

CRITICAL: The user's MOST RECENT messages are the current task. In long sessions, users change direction. If the recent messages ask for something different from the original request, the recent messages define what "on track" means.

If Claude is on track — working toward what the user's RECENT messages ask for — respond with:
{"should_respond": false, "message": "", "confidence": 0.9, "reasoning": "on track"}

If Claude is genuinely off track — doing workarounds instead of fixing root causes, going in circles, or ignoring explicit user instructions — respond with:
{"should_respond": true, "message": "You're [specific problem]. Instead, [specific redirect].", "confidence": 0.9, "reasoning": "off track because..."}

Only flag genuinely off-track behavior. Normal exploration, debugging, testing, and iteration are all fine. When in doubt, it's on track.`,
		"transcript_context": summary + "\n\n## TOOL CALL CLAUDE IS ABOUT TO MAKE:\nTool: " + toolName + "\nInput: " + truncateForInterrogate(toolInput, 500),
	})

	client := &http.Client{Timeout: s.evalTimeout}
	resp, err := client.Post(s.evaluatorURL+"/evaluate-idle", "application/json", bytes.NewReader(evalBody))
	if err != nil {
		return "" // Can't reach evaluator, let it through
	}
	defer resp.Body.Close()

	var rawResult struct {
		ShouldRespond bool    `json:"should_respond"`
		Message       string  `json:"message"`
		Confidence    float64 `json:"confidence"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&rawResult); err != nil {
		return ""
	}

	// should_respond=true means Claude is off track and needs redirecting
	if rawResult.ShouldRespond && rawResult.Message != "" && rawResult.Confidence >= s.interrogationConfidence {
		slog.Info("Interrogation: off track", "tool", toolName, "redirect", rawResult.Message)

		// Emit SSE event
		eventData, _ := json.Marshal(map[string]any{
			"timestamp":   time.Now().UTC().Format(time.RFC3339Nano),
			"action_type": "interrogate",
			"detail":      rawResult.Message,
			"tool_name":   toolName,
			"cwd":         cwd,
		})
		s.broker.Publish(SSEEvent{
			ID:   uuid.New().String()[:8],
			Type: "action",
			Data: string(eventData),
		})

		return rawResult.Message
	}

	return ""
}

// buildTranscriptSummary is a simplified version for interrogation context.
func buildTranscriptSummary(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}

	allLines := strings.Split(strings.TrimSpace(string(data)), "\n")

	// First user message
	var firstUser string
	for _, line := range allLines[:min(len(allLines), 50)] {
		var entry map[string]any
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			continue
		}
		msg, _ := entry["message"].(map[string]any)
		if msg == nil {
			continue
		}
		role, _ := msg["role"].(string)
		if role != "user" {
			continue
		}
		firstUser = extractText(msg)
		if firstUser != "" {
			break
		}
	}

	// Last 100 lines for recent context
	start := 0
	if len(allLines) > 100 {
		start = len(allLines) - 100
	}

	var recentUser, recentAssistant []string
	for _, line := range allLines[start:] {
		var entry map[string]any
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			continue
		}
		msg, _ := entry["message"].(map[string]any)
		if msg == nil {
			continue
		}
		role, _ := msg["role"].(string)
		text := extractText(msg)
		if text == "" {
			continue
		}
		switch role {
		case "user":
			recentUser = append(recentUser, text)
		case "assistant":
			recentAssistant = append(recentAssistant, text)
		}
	}

	var sb strings.Builder
	if firstUser != "" {
		sb.WriteString("## USER'S ORIGINAL REQUEST:\n")
		sb.WriteString(truncateForInterrogate(firstUser, 800))
		sb.WriteString("\n\n")
	}

	if len(recentUser) > 0 {
		sb.WriteString("## RECENT USER MESSAGES:\n")
		s := 0
		if len(recentUser) > 3 {
			s = len(recentUser) - 3
		}
		for _, m := range recentUser[s:] {
			sb.WriteString("- ")
			sb.WriteString(truncateForInterrogate(m, 200))
			sb.WriteString("\n")
		}
		sb.WriteString("\n")
	}

	if len(recentAssistant) > 0 {
		sb.WriteString("## RECENT ASSISTANT MESSAGES:\n")
		s := 0
		if len(recentAssistant) > 2 {
			s = len(recentAssistant) - 2
		}
		for _, m := range recentAssistant[s:] {
			sb.WriteString("[assistant]: ")
			sb.WriteString(truncateForInterrogate(m, 300))
			sb.WriteString("\n")
		}
	}

	return sb.String()
}

func extractText(msg map[string]any) string {
	switch content := msg["content"].(type) {
	case string:
		return content
	case []any:
		for _, item := range content {
			if m, ok := item.(map[string]any); ok {
				if t, ok := m["text"].(string); ok && t != "" {
					return t
				}
			}
		}
	}
	return ""
}

func truncateForInterrogate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max]
}

func (s *Server) handleHooksInstall(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	bin, err := os.Executable()
	if err != nil {
		http.Error(w, fmt.Sprintf("cannot find pilot binary: %v", err), http.StatusInternalServerError)
		return
	}

	home, _ := os.UserHomeDir()
	settingsPath := fmt.Sprintf("%s/.claude/settings.json", home)

	settings := make(map[string]any)
	if data, err := os.ReadFile(settingsPath); err == nil {
		_ = json.Unmarshal(data, &settings)
	}

	hooks, _ := settings["hooks"].(map[string]any)
	if hooks == nil {
		hooks = make(map[string]any)
	}

	hooks["PreToolUse"] = mergeHookEntries(hooks["PreToolUse"], map[string]any{
		"matcher": "^(Bash|Write|Edit|NotebookEdit|WebFetch|WebSearch)$",
		"hooks":   []any{map[string]any{"type": "command", "command": bin + " approve"}},
	})
	hooks["Stop"] = mergeHookEntries(hooks["Stop"], map[string]any{
		"hooks": []any{map[string]any{"type": "command", "command": bin + " on-stop"}},
	})

	settings["hooks"] = hooks
	data, _ := json.MarshalIndent(settings, "", "  ")
	os.MkdirAll(fmt.Sprintf("%s/.claude", home), 0755)
	if err := os.WriteFile(settingsPath, data, 0644); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "installed"})
}

func (s *Server) handleHooksUninstall(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	home, _ := os.UserHomeDir()
	settingsPath := fmt.Sprintf("%s/.claude/settings.json", home)

	data, err := os.ReadFile(settingsPath)
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"status": "uninstalled"})
		return
	}

	settings := make(map[string]any)
	if err := json.Unmarshal(data, &settings); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	hooks, _ := settings["hooks"].(map[string]any)
	if hooks != nil {
		removePilotHookEntries(hooks, "PreToolUse")
		removePilotHookEntries(hooks, "Stop")
		if len(hooks) == 0 {
			delete(settings, "hooks")
		}
	}

	out, _ := json.MarshalIndent(settings, "", "  ")
	os.WriteFile(settingsPath, out, 0644)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "uninstalled"})
}

func (s *Server) handleGetConfig(w http.ResponseWriter, r *http.Request) {
	cfg := config.Load()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(cfg)
}

func mergeHookEntries(existing any, pilotEntry map[string]any) []any {
	var result []any
	if arr, ok := existing.([]any); ok {
		for _, entry := range arr {
			entryJSON, _ := json.Marshal(entry)
			if !strings.Contains(string(entryJSON), "pilot approve") && !strings.Contains(string(entryJSON), "pilot on-stop") {
				result = append(result, entry)
			}
		}
	}
	result = append(result, pilotEntry)
	return result
}

func removePilotHookEntries(hooks map[string]any, key string) {
	arr, ok := hooks[key].([]any)
	if !ok {
		return
	}
	var filtered []any
	for _, entry := range arr {
		entryJSON, _ := json.Marshal(entry)
		if !strings.Contains(string(entryJSON), "pilot approve") && !strings.Contains(string(entryJSON), "pilot on-stop") {
			filtered = append(filtered, entry)
		}
	}
	if len(filtered) == 0 {
		delete(hooks, key)
	} else {
		hooks[key] = filtered
	}
}

func (s *Server) handleLogs(w http.ResponseWriter, r *http.Request) {
	logs := state.ReadLogs(200)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(logs)
}

func corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}
