package cmd

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"fmt"

	"github.com/NiallBrickell/pilot/internal/auth"
	"github.com/NiallBrickell/pilot/internal/config"
	"github.com/NiallBrickell/pilot/internal/state"

	"github.com/spf13/cobra"
)

func init() {
	rootCmd.AddCommand(&cobra.Command{
		Use:   "on-stop",
		Short: "Run as a Claude Code Stop hook (evaluates if Claude paused unnecessarily)",
		RunE:  runOnStop,
	})
}

func runOnStop(cmd *cobra.Command, args []string) error {
	if !auth.IsClaudeAuthed() {
		state.WriteLog("debug", "on-stop", "skipped: claude not authenticated")
		return nil
	}

	input, err := io.ReadAll(os.Stdin)
	if err != nil {
		state.WriteLog("error", "on-stop", "failed to read stdin: "+err.Error())
		return err
	}

	state.WriteLog("debug", "on-stop", fmt.Sprintf("hook fired, input length: %d bytes", len(input)))

	var hookData map[string]any
	if err := json.Unmarshal(input, &hookData); err != nil {
		state.WriteLog("warn", "on-stop", "failed to parse hook input: "+err.Error())
		hookData = map[string]any{}
	}

	// Log all keys received so we can debug field names
	var keys []string
	for k := range hookData {
		keys = append(keys, k)
	}
	state.WriteLog("debug", "on-stop", fmt.Sprintf("hook input keys: %v", keys))

	transcriptPath, _ := hookData["transcript_path"].(string)
	lastMessage, _ := hookData["last_assistant_message"].(string)
	sessionCwd, _ := hookData["cwd"].(string)
	if sessionCwd == "" {
		sessionCwd, _ = os.Getwd()
	}
	sessionID, _ := hookData["session_id"].(string)

	state.WriteLog("debug", "on-stop", fmt.Sprintf("transcript=%q lastMsg=%d chars cwd=%q session=%q",
		transcriptPath, len(lastMessage), sessionCwd, sessionID))

	// Build structured context from transcript + last message
	var context string
	if transcriptPath != "" {
		if c, err := buildConversationSummary(transcriptPath); err == nil && c != "" {
			context = c
			state.WriteLog("debug", "on-stop", fmt.Sprintf("built conversation summary: %d chars", len(c)))
		} else if err != nil {
			state.WriteLog("warn", "on-stop", "failed to build summary: "+err.Error())
		}
	}
	// Append last_assistant_message (most recent, may not be in transcript yet)
	if lastMessage != "" {
		context = context + "\n## CLAUDE'S FINAL MESSAGE (what it just said before stopping):\n" + lastMessage
	}
	if context == "" {
		context = "No transcript available"
		state.WriteLog("debug", "on-stop", "no context available, using fallback")
	}

	cfg := config.Load()

	// Try server first (semaphore-limited, emits SSE events)
	if ok := evaluateIdleViaServer(cfg, context, sessionCwd, sessionID); ok {
		return nil
	}

	// Serve not running — just let Claude stop normally, don't hang
	state.WriteLog("warn", "on-stop", "serve not running, skipping idle evaluation")
	slog.Warn("pilot: serve not running, skipping idle evaluation")
	return nil
}

func evaluateIdleViaServer(cfg *config.PilotConfig, context, cwd, sessionID string) bool {
	body, _ := json.Marshal(map[string]any{
		"transcript_context": context,
		"cwd":                cwd,
		"session_id":         sessionID,
	})

	client := &http.Client{Timeout: 15 * time.Second}
	state.WriteLog("debug", "on-stop", fmt.Sprintf("posting to %s/internal/evaluate-idle", config.SSEBaseURL(cfg)))
	resp, err := client.Post(config.SSEBaseURL(cfg)+"/internal/evaluate-idle", "application/json", bytes.NewReader(body))
	if err != nil {
		state.WriteLog("error", "on-stop", "serve not reachable: "+err.Error())
		return false
	}
	defer resp.Body.Close()

	var result struct {
		ShouldRespond bool    `json:"should_respond"`
		Message       string  `json:"message"`
		Confidence    float64 `json:"confidence"`
		Reasoning     string  `json:"reasoning"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		state.WriteLog("error", "on-stop", "failed to decode evaluator response: "+err.Error())
		return false
	}

	state.WriteLog("debug", "on-stop", fmt.Sprintf("evaluator result: should_respond=%v confidence=%.2f reasoning=%q message=%q",
		result.ShouldRespond, result.Confidence, result.Reasoning, result.Message))

	now := time.Now().UTC()
	if result.ShouldRespond && result.Confidence >= cfg.General.ConfidenceThreshold {
		state.WriteLog("info", "on-stop", fmt.Sprintf("nudging: %q (confidence %.2f)", result.Message, result.Confidence))
		_ = state.RecordAction(state.PilotAction{
			Timestamp:  now,
			ActionType: state.AutoRespond,
			Detail:     result.Message,
			Confidence: &result.Confidence,
		})

		// Return block decision — Claude sees the reason and continues
		printJSON(map[string]any{
			"decision": "block",
			"reason":   result.Message,
		})
	} else {
		state.WriteLog("debug", "on-stop", fmt.Sprintf("not nudging: confidence=%.2f threshold=%.2f",
			result.Confidence, cfg.General.ConfidenceThreshold))
		_ = state.RecordAction(state.PilotAction{
			Timestamp:  now,
			ActionType: state.AutoRespondSkipped,
			Detail:     result.Reasoning,
			Confidence: &result.Confidence,
		})
	}

	return true
}

// buildConversationSummary reads the transcript and builds a structured summary:
// - User's original request (first user message)
// - All user messages (intent/corrections)
// - Recent assistant text (last few messages, skipping tool use noise)
func buildConversationSummary(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}

	allLines := strings.Split(strings.TrimSpace(string(data)), "\n")
	// Only parse last 200 lines to keep it fast for long sessions
	startLine := 0
	if len(allLines) > 200 {
		startLine = len(allLines) - 200
	}
	lines := allLines[startLine:]

	type message struct {
		role string
		text string
	}

	var allMessages []message

	for _, line := range lines {
		var entry map[string]any
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			continue
		}

		msg, _ := entry["message"].(map[string]any)
		if msg == nil {
			continue
		}

		role, _ := msg["role"].(string)
		if role == "" {
			continue
		}

		var text string
		switch content := msg["content"].(type) {
		case string:
			text = content
		case []any:
			var texts []string
			for _, item := range content {
				if m, ok := item.(map[string]any); ok {
					if t, ok := m["text"].(string); ok && t != "" {
						texts = append(texts, t)
					}
				}
			}
			text = strings.Join(texts, "\n")
		}

		if text == "" {
			continue
		}

		allMessages = append(allMessages, message{role: role, text: text})
	}

	if len(allMessages) == 0 {
		return "", nil
	}

	var sb strings.Builder

	// Section 1: User's original request (scan from beginning of file, not just tail)
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
		var text string
		switch content := msg["content"].(type) {
		case string:
			text = content
		case []any:
			for _, item := range content {
				if m, ok := item.(map[string]any); ok {
					if t, ok := m["text"].(string); ok {
						text = t
						break
					}
				}
			}
		}
		if text != "" {
			sb.WriteString("## USER'S ORIGINAL REQUEST:\n")
			sb.WriteString(truncateStr(text, 1000))
			sb.WriteString("\n\n")
			break
		}
	}

	// Section 2: Recent user messages (last 5 — shows recent intent/corrections)
	var userMsgs []message
	for _, m := range allMessages {
		if m.role == "user" {
			userMsgs = append(userMsgs, m)
		}
	}
	if len(userMsgs) > 1 {
		sb.WriteString("## RECENT USER MESSAGES:\n")
		start := 0
		if len(userMsgs) > 5 {
			start = len(userMsgs) - 5
		}
		for _, m := range userMsgs[start:] {
			sb.WriteString("- ")
			sb.WriteString(truncateStr(m.text, 200))
			sb.WriteString("\n")
		}
		sb.WriteString("\n")
	}

	// Section 3: Last 3 assistant messages
	sb.WriteString("## RECENT ASSISTANT MESSAGES:\n")
	var assistantMsgs []message
	for _, m := range allMessages {
		if m.role == "assistant" {
			assistantMsgs = append(assistantMsgs, m)
		}
	}
	start := 0
	if len(assistantMsgs) > 3 {
		start = len(assistantMsgs) - 3
	}
	for _, m := range assistantMsgs[start:] {
		sb.WriteString("[assistant]: ")
		sb.WriteString(truncateStr(m.text, 300))
		sb.WriteString("\n")
	}

	return sb.String(), nil
}

func truncateStr(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max]
}
