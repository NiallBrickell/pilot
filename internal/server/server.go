package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/NiallBrickell/pilot/internal/anthropic"
	"github.com/NiallBrickell/pilot/internal/approve"
	"github.com/NiallBrickell/pilot/internal/config"
	pilothooks "github.com/NiallBrickell/pilot/internal/hooks"
	"github.com/NiallBrickell/pilot/internal/paths"
	"github.com/NiallBrickell/pilot/internal/state"
	pilottranscript "github.com/NiallBrickell/pilot/internal/transcript"

	"github.com/google/uuid"
)

// Server is a lightweight HTTP server providing SSE events and grace-period control.
type Server struct {
	broker                  *Broker
	pending                 *PendingStore
	port                    int
	srv                     *http.Server
	evalSem                 chan struct{} // semaphore to limit concurrent approval evaluations
	idleSem                 chan struct{} // separate semaphore for idle evals (won't block approvals)
	toolCounts              sync.Map      // session_id → *toolCounter
	ai                      *anthropic.Client
	evalTimeout             time.Duration
	interrogationConfidence float64
	interrogationModel      string
	interrogationEnabled    bool
}

// toolCounter tracks tool calls per session for checkpoint logic.
type toolCounter struct {
	mu              sync.Mutex
	countSinceUser  int
	lastUserMsgHash string    // detect new user messages
	lastSeen        time.Time // for stale entry cleanup
}

// shouldInterrogate returns true if this tool call should include a
// context-aware checkpoint (is Claude still on track?).
// Fires on: 1st, 5th, then every 25th tool call after each user message.
func (tc *toolCounter) shouldInterrogate(userMsgHash string) bool {
	tc.mu.Lock()
	defer tc.mu.Unlock()

	tc.lastSeen = time.Now()
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
		evalTimeout = 15 * time.Second
	}
	interrogationConf := cfg.General.InterrogationConfidence
	if interrogationConf <= 0 {
		interrogationConf = 0.7
	}
	interrogationModel := cfg.General.InterrogationModel
	if interrogationModel == "" {
		interrogationModel = "claude-sonnet-4-6"
	}
	interrogationEnabled := cfg.General.IsInterrogationEnabled()

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
		idleSem:                 make(chan struct{}, 2),
		evalTimeout:             evalTimeout,
		interrogationConfidence: interrogationConf,
		interrogationModel:      interrogationModel,
		interrogationEnabled:    interrogationEnabled,
	}
}

// SetAI sets the Anthropic API client used for evaluations.
func (s *Server) SetAI(client *anthropic.Client) {
	s.ai = client
}

// EvalTimeout returns the configured evaluation timeout.
func (s *Server) EvalTimeout() time.Duration {
	return s.evalTimeout
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
	mux.HandleFunc("/internal/interrogate", s.handleInterrogate)
	mux.HandleFunc("/internal/evaluate-idle", s.handleEvaluateIdle)
	mux.HandleFunc("/hooks/install", s.handleHooksInstall)
	mux.HandleFunc("/hooks/uninstall", s.handleHooksUninstall)
	mux.HandleFunc("/config", s.handleGetConfig)
	mux.HandleFunc("/config/prompts-status", s.handlePromptsStatus)
	mux.HandleFunc("/config/reset-prompts", s.handleResetPrompts)
	mux.HandleFunc("/logs", s.handleLogs)
	mux.HandleFunc("/internal/profile", s.handleProfile)

	s.srv = &http.Server{
		Addr:    fmt.Sprintf(":%d", s.port),
		Handler: corsMiddleware(mux),
	}

	// Periodically evict stale toolCounts entries (sessions inactive >1h)
	go func() {
		ticker := time.NewTicker(10 * time.Minute)
		defer ticker.Stop()
		for range ticker.C {
			cutoff := time.Now().Add(-1 * time.Hour)
			s.toolCounts.Range(func(key, value any) bool {
				tc := value.(*toolCounter)
				tc.mu.Lock()
				stale := tc.lastSeen.Before(cutoff)
				tc.mu.Unlock()
				if stale {
					s.toolCounts.Delete(key)
				}
				return true
			})
		}
	}()

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

	hookStatus := pilothooks.CheckInstalled()

	// Merge into response
	raw, _ := json.Marshal(ps)
	var result map[string]any
	json.Unmarshal(raw, &result)
	result["hooks_installed"] = hookStatus.Installed
	result["claude_hooks_installed"] = hookStatus.ClaudeInstalled
	result["codex_hooks_installed"] = hookStatus.CodexInstalled
	cfg := config.Load()
	result["monthly_usage"] = state.ReadMonthlyUsage(time.Now())
	result["monthly_spend_cap_usd"] = cfg.General.MonthlySpendCapUSD

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

// handleEvaluate runs approval evaluation (layers 1-3) via the Anthropic API.
func (s *Server) handleEvaluate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		Runtime        string `json:"runtime"`
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

	slog.Debug("Evaluate request", "tool", req.ToolName, "cwd", req.Cwd, "session", req.SessionID)

	evalStart := time.Now()

	cfg := config.Load()

	// Auto-approve mode: skip all approval evaluation (for autonomous/sandboxed use).
	// Interrogation still ran above.
	if cfg.General.AutoApproveAll {
		durationMs := float64(time.Since(evalStart).Microseconds()) / 1000.0
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"decision":    "approve",
			"reason":      "auto_approve_all",
			"source":      "config",
			"duration_ms": durationMs,
			"tool_name":   req.ToolName,
			"cwd":         req.Cwd,
			"session_id":  req.SessionID,
		})
		return
	}

	// Layer 1 + 2: Check runtime settings and pilot rules before hitting LLM.
	if decision := approve.EvaluateForRuntime(cfg, req.Runtime, req.ToolName, req.ToolInput, req.Cwd); decision != nil {
		durationMs := float64(time.Since(evalStart).Microseconds()) / 1000.0
		slog.Debug("Evaluate completed (local)", "tool", req.ToolName, "source", decision.Source, "duration_ms", durationMs)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"decision":    decision.Action,
			"reason":      decision.Reason,
			"source":      decision.Source,
			"duration_ms": durationMs,
			"tool_name":   req.ToolName,
			"cwd":         req.Cwd,
			"session_id":  req.SessionID,
		})
		return
	}

	// Layer 3: Call Anthropic API directly
	if s.ai == nil {
		durationMs := float64(time.Since(evalStart).Microseconds()) / 1000.0
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"decision":    "deny",
			"reason":      "anthropic API client not configured",
			"duration_ms": durationMs,
			"tool_name":   req.ToolName,
			"cwd":         req.Cwd,
			"session_id":  req.SessionID,
		})
		return
	}

	model := cfg.General.Model
	if model == "" {
		model = "claude-haiku-4-5"
	}
	if capReached, usage := evaluatorSpendCapReached(cfg); capReached {
		durationMs := float64(time.Since(evalStart).Microseconds()) / 1000.0
		reason := fmt.Sprintf("monthly evaluator spend cap reached ($%.2f / $%.2f)", usage.EstimatedCostUSD, cfg.General.MonthlySpendCapUSD)
		slog.Warn("Evaluate skipped: spend cap reached", "tool", req.ToolName, "spent", usage.EstimatedCostUSD, "cap", cfg.General.MonthlySpendCapUSD)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"decision":    "ask",
			"reason":      reason,
			"duration_ms": durationMs,
			"tool_name":   req.ToolName,
			"cwd":         req.Cwd,
			"session_id":  req.SessionID,
		})
		return
	}

	s.evalSem <- struct{}{}
	defer func() { <-s.evalSem }()

	// Retry only errors Anthropic documents as transient, with a fresh deadline.
	// Billing/auth/validation failures cannot recover on the next call; retrying
	// those once per tool invocation caused the exhausted-credit request storm.
	var evalResult *anthropic.ApprovalResult
	var err error
	for attempt := range 2 {
		ctx, cancel := context.WithTimeout(r.Context(), s.evalTimeout)
		evalResult, err = s.ai.EvaluateApproval(ctx, cfg.Prompts.Approval, req.ToolName, req.ToolInput, model)
		cancel()
		if err == nil {
			break
		}
		if attempt == 1 || !anthropic.IsRetryable(err) {
			break
		}
		delay := evaluatorRetryDelay(err)
		slog.Warn("Transient Anthropic API error (approval), retrying", "error", err, "tool", req.ToolName, "delay", delay)
		time.Sleep(delay)
	}
	if err != nil {
		// Evaluator unavailable even after retry. Apply the same deterministic
		// boundary used in normal mode: routine calls approve; danger markers or
		// input that cannot be inspected still fall through to the user.
		durationMs := float64(time.Since(evalStart).Microseconds()) / 1000.0
		decision := "ask"
		reason := "pilot: evaluator unavailable and call is dangerous or ambiguous — confirm manually"
		if approve.FailOpenDecision(req.ToolName, req.ToolInput) == "approve" {
			decision = "approve"
			reason = "pilot: evaluator unavailable (API error), auto-approved by fail-open rules"
		}
		slog.Warn("Anthropic API error (approval), failing open", "error", err, "decision", decision, "tool", req.ToolName, "duration_ms", durationMs)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"decision":    decision,
			"reason":      reason,
			"source":      "fail_open",
			"duration_ms": durationMs,
			"tool_name":   req.ToolName,
			"cwd":         req.Cwd,
			"session_id":  req.SessionID,
		})
		return
	}
	recordEvaluatorUsage("approval", model, cfg, evalResult.Usage)

	durationMs := float64(time.Since(evalStart).Microseconds()) / 1000.0

	decision := "deny"
	actionType := "escalate"
	confidence := 0.0
	if evalResult.Decision == anthropic.Approve {
		decision = "approve"
		actionType = "auto_approve"
		confidence = 1.0
	}

	slog.Debug("Evaluate completed (haiku)", "tool", req.ToolName, "decision", decision, "duration_ms", durationMs)

	// Emit SSE event
	now := time.Now().UTC()
	eventData, _ := json.Marshal(map[string]any{
		"timestamp":   now.Format(time.RFC3339Nano),
		"action_type": actionType,
		"detail":      fmt.Sprintf("%s: %s", req.ToolName, evalResult.Reason),
		"confidence":  confidence,
		"duration_ms": durationMs,
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
		"decision":    decision,
		"reason":      evalResult.Reason,
		"confidence":  confidence,
		"duration_ms": durationMs,
		"tool_name":   req.ToolName,
		"cwd":         req.Cwd,
		"session_id":  req.SessionID,
	})
}

// handleProfile returns aggregated evaluation timing stats.
func (s *Server) handleProfile(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	bySource := state.ReadProfile(0)
	overall := state.ReadProfileAll(0)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"by_source": bySource,
		"overall":   overall,
	})
}

// handleInterrogate is a standalone endpoint for the interrogation hook.
// It runs independently of the approval flow on ALL tool calls.
func (s *Server) handleInterrogate(w http.ResponseWriter, r *http.Request) {
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

	// Quick path: interrogation disabled or missing context
	if !s.interrogationEnabled || req.SessionID == "" || req.TranscriptPath == "" {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"decision": "allow"})
		return
	}

	// Check if this tool call should trigger an interrogation checkpoint
	raw, _ := s.toolCounts.LoadOrStore(req.SessionID, &toolCounter{})
	tc := raw.(*toolCounter)
	if !tc.shouldInterrogate(req.UserMsgHash) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"decision": "allow"})
		return
	}

	slog.Info("Interrogating", "tool", req.ToolName, "session", req.SessionID)
	redirect := s.interrogate(req.TranscriptPath, req.ToolName, req.ToolInput, req.Cwd)
	if redirect != "" {
		slog.Info("Interrogation: redirecting", "tool", req.ToolName, "redirect", redirect[:min(len(redirect), 100)])
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

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"decision": "allow"})
}

// handleEvaluateIdle runs idle evaluation via the Anthropic API.
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

	if s.ai == nil {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"should_respond": false,
			"message":        "",
			"confidence":     0,
			"reasoning":      "anthropic API client not configured",
		})
		return
	}

	model := cfg.General.Model
	if model == "" {
		model = "claude-haiku-4-5"
	}
	if capReached, usage := evaluatorSpendCapReached(cfg); capReached {
		slog.Warn("Idle evaluation skipped: spend cap reached", "spent", usage.EstimatedCostUSD, "cap", cfg.General.MonthlySpendCapUSD)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"should_respond": false,
			"message":        "",
			"confidence":     0,
			"reasoning":      fmt.Sprintf("monthly evaluator spend cap reached ($%.2f / $%.2f)", usage.EstimatedCostUSD, cfg.General.MonthlySpendCapUSD),
		})
		return
	}

	s.idleSem <- struct{}{}
	defer func() { <-s.idleSem }()

	ctx, cancel := context.WithTimeout(r.Context(), s.evalTimeout)
	defer cancel()

	idleResult, err := s.ai.EvaluateIdle(ctx, cfg.Prompts.AutoRespond, req.TranscriptContext, model)
	if err != nil {
		slog.Warn("Anthropic API error (idle)", "error", err)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"should_respond": false,
			"message":        "",
			"confidence":     0,
			"reasoning":      fmt.Sprintf("evaluation error: %v", err),
		})
		return
	}
	recordEvaluatorUsage("idle", model, cfg, idleResult.Usage)

	// Emit SSE event
	now := time.Now().UTC()
	actionType := "auto_respond_skipped"
	if idleResult.ShouldRespond {
		actionType = "auto_respond"
	}
	eventData, _ := json.Marshal(map[string]any{
		"timestamp":   now.Format(time.RFC3339Nano),
		"action_type": actionType,
		"detail":      idleResult.Message,
		"confidence":  idleResult.Confidence,
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
		"should_respond": idleResult.ShouldRespond,
		"message":        idleResult.Message,
		"confidence":     idleResult.Confidence,
		"reasoning":      idleResult.Reasoning,
	})
}

// interrogate reads the conversation context and checks if the agent is still
// on track. Returns a redirect message if off-track, empty string if fine.
func (s *Server) interrogate(transcriptPath, toolName, toolInput, cwd string) string {
	// Build conversation summary (reuse the same logic as on-stop)
	summary := buildTranscriptSummary(transcriptPath)
	if summary == "" {
		slog.Debug("Interrogation skipped: no transcript context", "tool", toolName)
		return ""
	}

	slog.Info("Interrogation context",
		"tool", toolName,
		"summary_len", len(summary),
		"recent_user_preview", truncateForInterrogate(summary[max(0, len(summary)-500):], 500),
	)

	systemPrompt := `You are a safety net for an AI coding assistant. You RARELY intervene.

You'll see conversation context and the tool call the assistant is about to make. Only flag SERIOUSLY off-track behaviour.

INTERVENE (respond with should_respond: true) ONLY when:
- The assistant is completely ignoring what the user explicitly asked for
- The assistant is stuck in a loop doing the same thing repeatedly
- The assistant is working on something the user explicitly said NOT to do
- The assistant is making a fundamental architectural mistake the user already corrected

DO NOT INTERVENE for:
- Normal implementation decisions (choosing an email address, picking a config value, etc.)
- Minor deviations that are part of working toward the goal
- The assistant exploring or debugging before implementing
- The assistant doing things in a different order than you'd expect
- Anything that's a reasonable interpretation of the user's request
- The assistant doing what the user asked, even if it looks risky (e.g. hardcoding test values, modifying production code for local testing) — if the user said to do it and the assistant agreed, it's on track
- Subagent exploration (Read, Grep, find commands to understand codebase)

The bar for intervention is HIGH. If you're not 90%+ sure Claude is seriously off track, say it's on track.

If on track: {"should_respond": false, "message": "", "confidence": 0.9, "reasoning": "on track"}
If seriously off track: {"should_respond": true, "message": "Stop — [what's wrong and what to do instead]", "confidence": 0.95, "reasoning": "..."}`

	transcriptContext := summary + "\n\n## TOOL CALL CLAUDE IS ABOUT TO MAKE:\nTool: " + toolName + "\nInput: " + truncateForInterrogate(toolInput, 500)

	if s.ai == nil {
		return ""
	}

	cfg := config.Load()
	if capReached, usage := evaluatorSpendCapReached(cfg); capReached {
		slog.Warn("Interrogation skipped: spend cap reached", "spent", usage.EstimatedCostUSD, "cap", cfg.General.MonthlySpendCapUSD)
		return ""
	}

	ctx, cancel := context.WithTimeout(context.Background(), s.evalTimeout)
	defer cancel()

	rawResult, err := s.ai.EvaluateIdle(ctx, systemPrompt, transcriptContext, s.interrogationModel)
	if err != nil {
		slog.Warn("Interrogation API error — letting tool through (approval already passed)", "error", err)
		return ""
	}
	recordEvaluatorUsage("interrogate", s.interrogationModel, cfg, rawResult.Usage)

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

func evaluatorSpendCapReached(cfg *config.PilotConfig) (bool, state.MonthlyUsage) {
	usage := state.ReadMonthlyUsage(time.Now())
	capUSD := cfg.General.MonthlySpendCapUSD
	return capUSD > 0 && usage.EstimatedCostUSD >= capUSD, usage
}

func evaluatorRetryDelay(err error) time.Duration {
	const (
		defaultDelay = 300 * time.Millisecond
		maxDelay     = 5 * time.Second
	)
	delay := anthropic.RetryDelay(err)
	if delay <= 0 {
		return defaultDelay
	}
	return min(delay, maxDelay)
}

func recordEvaluatorUsage(kind, model string, cfg *config.PilotConfig, usage anthropic.Usage) {
	if usage.TotalInputTokens() <= 0 && usage.OutputTokens <= 0 {
		return
	}
	costUSD := estimateUsageCostUSD(cfg, usage)
	slog.Debug("Anthropic evaluator usage",
		"kind", kind,
		"model", model,
		"input_tokens", usage.InputTokens,
		"cache_creation_input_tokens", usage.CacheCreationInputTokens,
		"cache_read_input_tokens", usage.CacheReadInputTokens,
		"output_tokens", usage.OutputTokens,
		"estimated_cost_usd", costUSD,
	)
	if err := state.RecordUsage(state.UsageRecord{
		Timestamp:        time.Now().UTC(),
		Kind:             kind,
		Model:            model,
		InputTokens:      uint64(max(usage.TotalInputTokens(), 0)),
		OutputTokens:     uint64(max(usage.OutputTokens, 0)),
		EstimatedCostUSD: costUSD,
	}); err != nil {
		slog.Warn("Failed to record Anthropic usage", "error", err, "kind", kind, "model", model)
	}
}

func estimateUsageCostUSD(cfg *config.PilotConfig, usage anthropic.Usage) float64 {
	inputCost := cfg.General.InputCostPerMTokUSD
	if inputCost <= 0 {
		inputCost = config.DefaultInputCostPerMTokUSD
	}
	outputCost := cfg.General.OutputCostPerMTokUSD
	if outputCost <= 0 {
		outputCost = config.DefaultOutputCostPerMTokUSD
	}
	// Anthropic's five-minute cache charges writes at 1.25x base input and
	// reads at 0.1x. Keep all input categories in the spend estimate so the
	// monthly cap remains authoritative once caching starts taking effect.
	return (float64(max(usage.InputTokens, 0))/1_000_000)*inputCost +
		(float64(max(usage.CacheCreationInputTokens, 0))/1_000_000)*inputCost*1.25 +
		(float64(max(usage.CacheReadInputTokens, 0))/1_000_000)*inputCost*0.1 +
		(float64(max(usage.OutputTokens, 0))/1_000_000)*outputCost
}

// transcriptReadLimit caps how much of the transcript file we read into memory.
// The head (first user message) and tail (recent turns) are read separately
// so we never load the entire file — transcripts can grow to hundreds of MB
// and loading them fully caused catastrophic memory usage (200GB+) when
// multiple concurrent interrogation requests were in flight.
const transcriptReadLimit = 256 * 1024 // 256KB per read

// buildTranscriptSummary reads the head and tail of a transcript file to
// extract the first user message and recent conversation turns.
func buildTranscriptSummary(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()

	fi, err := f.Stat()
	if err != nil {
		return ""
	}
	size := fi.Size()

	// --- Head: read first 256KB to find the original user request ---
	headLen := min(int64(transcriptReadLimit), size)
	headBuf := make([]byte, headLen)
	if _, err := f.ReadAt(headBuf, 0); err != nil && err != io.EOF {
		return ""
	}

	var firstUser string
	linesScanned := 0
	forEachLine(headBuf, func(line []byte) bool {
		linesScanned++
		if linesScanned > 50 {
			return false
		}
		var entry map[string]any
		if json.Unmarshal(line, &entry) != nil {
			return true
		}
		msg, ok := pilottranscript.ParseLine(entry)
		if ok && msg.Role == "user" {
			firstUser = msg.Text
			return firstUser == "" // stop if found
		}
		return true
	})

	// --- Tail: read last 256KB for recent conversation turns ---
	tailOffset := int64(0)
	tailLen := size
	if size > transcriptReadLimit {
		tailOffset = size - transcriptReadLimit
		tailLen = transcriptReadLimit
	}
	tailBuf := make([]byte, tailLen)
	if _, err := f.ReadAt(tailBuf, tailOffset); err != nil && err != io.EOF {
		return ""
	}

	var recentUser, recentAssistant []string
	forEachLine(tailBuf, func(line []byte) bool {
		var entry map[string]any
		if json.Unmarshal(line, &entry) != nil {
			return true
		}
		msg, ok := pilottranscript.ParseLine(entry)
		if !ok {
			return true
		}
		switch msg.Role {
		case "user":
			recentUser = append(recentUser, msg.Text)
		case "assistant":
			recentAssistant = append(recentAssistant, msg.Text)
		}
		return true
	})

	var sb strings.Builder
	if firstUser != "" {
		sb.WriteString("## USER'S ORIGINAL REQUEST:\n")
		sb.WriteString(truncateForInterrogate(firstUser, 800))
		sb.WriteString("\n\n")
	}

	if len(recentUser) > 0 {
		sb.WriteString("## RECENT USER MESSAGES:\n")
		s := 0
		if len(recentUser) > 5 {
			s = len(recentUser) - 5
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
		if len(recentAssistant) > 3 {
			s = len(recentAssistant) - 3
		}
		for _, m := range recentAssistant[s:] {
			sb.WriteString("[assistant]: ")
			sb.WriteString(truncateForInterrogate(m, 300))
			sb.WriteString("\n")
		}
	}

	return sb.String()
}

// forEachLine calls fn for each newline-delimited segment in data.
// fn returns false to stop iteration.
func forEachLine(data []byte, fn func(line []byte) bool) {
	start := 0
	for i, b := range data {
		if b == '\n' {
			if i > start {
				if !fn(data[start:i]) {
					return
				}
			}
			start = i + 1
		}
	}
	if start < len(data) {
		fn(data[start:])
	}
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

	if err := pilothooks.InstallAll(bin); err != nil {
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

	if err := pilothooks.UninstallAll(); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "uninstalled"})
}

func (s *Server) handleGetConfig(w http.ResponseWriter, r *http.Request) {
	cfg := config.Load()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(cfg)
}

// handlePromptsStatus returns whether the user's prompts are up-to-date, behind
// (embedded default changed and they haven't edited), or customised.
func (s *Server) handlePromptsStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	status, err := paths.PromptsStatusOf(config.EmbeddedConfig(), config.ShippedPromptHashes())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(status)
}

// handleResetPrompts force-replaces the [prompts] section with the embedded
// default. Used by the dashboard when the user explicitly clicks upgrade/revert.
// Reloads the in-process config so the change takes effect without restart.
func (s *Server) handleResetPrompts(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	result, err := paths.ResetPromptsToDefault(config.EmbeddedConfig())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if result.Upgraded {
		slog.Info("Prompts reset to embedded default", "backup", result.BackupPath)
	}
	status, _ := paths.PromptsStatusOf(config.EmbeddedConfig(), config.ShippedPromptHashes())
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"upgraded":    result.Upgraded,
		"backup_path": result.BackupPath,
		"reason":      result.Reason,
		"status":      status,
	})
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
