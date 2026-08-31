package cmd

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/NiallBrickell/pilot/internal/config"
	"github.com/NiallBrickell/pilot/internal/paths"
	"github.com/NiallBrickell/pilot/internal/state"

	"github.com/spf13/cobra"
)

// hookResponse / preToolUseOutput are the typed PreToolUse answer shape shared
// with the interrogation hook.
type hookResponse struct {
	HookSpecificOutput preToolUseOutput `json:"hookSpecificOutput"`
}

type preToolUseOutput struct {
	HookEventName            string  `json:"hookEventName"`
	PermissionDecision       string  `json:"permissionDecision"`
	PermissionDecisionReason *string `json:"permissionDecisionReason,omitempty"`
}

type hookRuntime string

const (
	runtimeClaude hookRuntime = "claude"
	runtimeCodex  hookRuntime = "codex"
)

// Claude Code hook events Pilot participates in. Each maps to one command so
// a settings file shows at a glance which role a hook entry plays; the handler
// still dispatches on the event name Claude Code sends, so a mismatched entry
// behaves according to the event rather than the command.
const (
	eventPreToolUse        = "PreToolUse"
	eventPermissionRequest = "PermissionRequest"
	eventPermissionDenied  = "PermissionDenied"
)

// Retry decisions exchanged with pilot serve; mirror internal/server/retry.go.
const (
	retryAllow = "allow"
	retryAsk   = "ask"

	retryStagePreToolUse        = "pre_tool_use"
	retryStagePermissionRequest = "permission_request"
)

func init() {
	rootCmd.AddCommand(&cobra.Command{
		Use:   "approve",
		Short: "Run as a Claude Code PermissionRequest hook",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runApproveForRuntime(runtimeClaude, eventPermissionRequest)
		},
	})
	rootCmd.AddCommand(&cobra.Command{
		Use:   "on-denied",
		Short: "Run as a Claude Code PermissionDenied hook (auto-mode classifier fallback)",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runApproveForRuntime(runtimeClaude, eventPermissionDenied)
		},
	})
	rootCmd.AddCommand(&cobra.Command{
		Use:   "pre-approve",
		Short: "Run as a Claude Code PreToolUse hook (applies Pilot's decision to a retried call)",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runApproveForRuntime(runtimeClaude, eventPreToolUse)
		},
	})
	rootCmd.AddCommand(&cobra.Command{
		Use:   "codex-approve",
		Short: "Run as a Codex PreToolUse or PermissionRequest hook",
		RunE:  func(cmd *cobra.Command, args []string) error { return runApproveForRuntime(runtimeCodex, "") },
	})
}

// hookInput is the subset of Claude Code / Codex hook input Pilot uses.
type hookInput struct {
	Event          string
	ToolName       string
	ToolInput      string
	Cwd            string
	SessionID      string
	TranscriptPath string
	Reason         string // PermissionDenied only
	raw            map[string]any
}

func parseHookInput(input []byte, defaultEvent string) hookInput {
	var toolInfo map[string]any
	if err := json.Unmarshal(input, &toolInfo); err != nil {
		toolInfo = map[string]any{}
	}

	h := hookInput{raw: toolInfo}
	h.ToolName, _ = toolInfo["tool_name"].(string)
	if h.ToolName == "" {
		h.ToolName = "unknown"
	}
	h.Event, _ = toolInfo["hook_event_name"].(string)
	if h.Event == "" {
		h.Event, _ = toolInfo["hookEventName"].(string)
	}
	if h.Event == "" {
		h.Event = defaultEvent
	}

	switch v := toolInfo["tool_input"].(type) {
	case string:
		h.ToolInput = v
	case map[string]any, []any:
		b, _ := json.Marshal(v)
		h.ToolInput = string(b)
	}

	h.Cwd, _ = toolInfo["cwd"].(string)
	if h.Cwd == "" {
		h.Cwd, _ = os.Getwd()
	}
	h.SessionID, _ = toolInfo["session_id"].(string)
	if h.SessionID == "" {
		h.SessionID, _ = toolInfo["turn_id"].(string)
	}
	h.TranscriptPath, _ = toolInfo["transcript_path"].(string)
	h.Reason, _ = toolInfo["reason"].(string)
	return h
}

func runApproveForRuntime(runtime hookRuntime, defaultEvent string) error {
	cliStart := time.Now()

	input, err := io.ReadAll(os.Stdin)
	if err != nil {
		return err
	}
	slog.Debug("Hook input", "input", string(input))
	h := parseHookInput(input, defaultEvent)

	// PreToolUse fires on every tool call, so it stays a local lookup: no
	// setup, no auth probe, no transcript read. It only answers when Pilot has
	// already decided this exact call after a classifier denial.
	if runtime == runtimeClaude && strings.EqualFold(h.Event, eventPreToolUse) {
		return runPreToolUseClaim(config.Load(), h)
	}

	paths.EnsureSetup(config.EmbeddedConfig())

	if runtime == runtimeCodex {
		state.WriteLog("debug", "codex-approve", fmt.Sprintf("tool=%s hookEvent=%q cwd=%q", h.ToolName, h.Event, h.Cwd))
	}

	// Hash of last user message text to detect new user turns for interrogation.
	// Only read the tail of the transcript to avoid loading huge files into memory.
	var userMsgHash string
	if h.TranscriptPath != "" {
		userMsgHash = lastUserMsgHash(h.TranscriptPath)
	}

	cfg := config.Load()

	// A retry Pilot already decided arrives here when the PreToolUse hook asked
	// for a prompt (or when a permission rule prompted before PreToolUse could
	// answer). Honour that decision instead of evaluating the call twice.
	if runtime == runtimeClaude && strings.EqualFold(h.Event, eventPermissionRequest) {
		if handled, err := handlePermissionRequestRetry(cfg, h); handled {
			return err
		}
	}

	// Evaluate via serve. If serve isn't running, fail open — pilot is
	// effectively off, so the hook should be a silent no-op rather than
	// forcing the user to approve every command.
	result, ok := evaluateViaServer(cfg, runtime, h.ToolName, h.ToolInput, h.Cwd, h.SessionID, h.TranscriptPath, userMsgHash)
	if !ok {
		slog.Debug("pilot: serve not running, leaving hook undecided", "event", h.Event)
		return nil
	}

	return handleEvalResult(cfg, runtime, h, result, cliStart)
}

// postServe sends a JSON POST to pilot serve with the bearer token the server
// expects, when one is configured. Hook processes inherit Claude Code's
// environment, not the server's, so the token is resolved from the same
// ~/.pilot/.env the server reads (see config.ServerToken).
func postServe(cfg *config.PilotConfig, path string, body []byte, timeout time.Duration) (*http.Response, error) {
	req, err := http.NewRequest(http.MethodPost, config.SSEBaseURL(cfg)+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if token := config.ServerToken(); token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	client := &http.Client{Timeout: timeout}
	return client.Do(req)
}

// isHeadlessSession reports whether the hook runs under `claude -p`, where
// Claude Code has no permission prompt to show: a hook "ask" there is turned
// straight into a denial. Claude Code marks that entrypoint in the environment
// it passes to hooks.
func isHeadlessSession() bool {
	return os.Getenv("CLAUDE_CODE_ENTRYPOINT") == "sdk-cli"
}

type evalResult struct {
	Decision   string  `json:"decision"`
	Reason     string  `json:"reason"`
	Confidence float64 `json:"confidence"`
	Source     string  `json:"source"`      // "settings", "pilot", "haiku", "interrogate"
	DurationMs float64 `json:"duration_ms"` // server-side eval time
}

// evaluateViaServer calls the pilot serve process to evaluate.
func evaluateViaServer(cfg *config.PilotConfig, runtime hookRuntime, toolName, toolInput, cwd, sessionID, transcriptPath, userMsgHash string) (*evalResult, bool) {
	body, _ := json.Marshal(map[string]any{
		"runtime":         string(runtime),
		"tool_name":       toolName,
		"tool_input":      toolInput,
		"cwd":             cwd,
		"session_id":      sessionID,
		"transcript_path": transcriptPath,
		"user_msg_hash":   userMsgHash,
	})

	// Must outlast serve's worst case: two evaluator attempts at evaluator_timeout_ms
	// (default 15s) plus retry backoff. If this fires first the hook falls back to
	// the unconditional "serve not running" allow, skipping the danger-marker check.
	resp, err := postServe(cfg, "/internal/evaluate", body, 45*time.Second)
	if err != nil {
		slog.Debug("Serve not reachable, falling back to local eval", "error", err)
		return nil, false
	}
	defer resp.Body.Close()

	var result evalResult
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, false
	}
	return &result, true
}

// handleEvalResult converts a server evaluation result into the hook response.
func handleEvalResult(cfg *config.PilotConfig, runtime hookRuntime, h hookInput, result *evalResult, cliStart time.Time) error {
	if runtime == runtimeCodex {
		return handleCodexEvalResult(cfg, h.Event, result, h.ToolName, h.ToolInput, h.Cwd, h.SessionID, cliStart)
	}
	if strings.EqualFold(h.Event, eventPermissionDenied) {
		return handlePermissionDeniedResult(cfg, h, result, cliStart)
	}
	return handlePermissionRequestResult(cfg, result, h.ToolName, h.ToolInput, h.Cwd, h.SessionID, cliStart)
}

// retryRecord is the server's answer to a claim/nudge on /internal/retry.
type retryRecord struct {
	Decision  string `json:"decision"`
	Reason    string `json:"reason"`
	ToolName  string `json:"tool_name"`
	ToolInput string `json:"tool_input"`
}

// retryRequest talks to pilot serve's retry hand-off. The timeout is short:
// PreToolUse runs on every tool call and must never make a session wait on a
// server that has gone away. A nil result means "nothing pending" or "serve
// unreachable"; both fail open to Claude Code's normal behaviour.
func retryRequest(cfg *config.PilotConfig, payload map[string]any, timeout time.Duration) *retryRecord {
	body, _ := json.Marshal(payload)
	resp, err := postServe(cfg, "/internal/retry", body, timeout)
	if err != nil {
		slog.Debug("Serve not reachable for retry hand-off", "error", err)
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		slog.Debug("Retry hand-off rejected", "status", resp.StatusCode)
		return nil
	}
	var rec retryRecord
	if err := json.NewDecoder(resp.Body).Decode(&rec); err != nil || rec.Decision == "" {
		return nil
	}
	return &rec
}

func registerRetry(cfg *config.PilotConfig, h hookInput, decision, reason string) bool {
	if h.SessionID == "" {
		return false
	}
	body, _ := json.Marshal(map[string]any{
		"action":     "register",
		"session_id": h.SessionID,
		"tool_name":  h.ToolName,
		"tool_input": h.ToolInput,
		"decision":   decision,
		"reason":     reason,
	})
	resp, err := postServe(cfg, "/internal/retry", body, 2*time.Second)
	if err != nil {
		slog.Debug("Serve not reachable to register retry", "error", err)
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

func claimRetry(cfg *config.PilotConfig, h hookInput, stage string) *retryRecord {
	if h.SessionID == "" {
		return nil
	}
	return retryRequest(cfg, map[string]any{
		"action":     "claim",
		"stage":      stage,
		"session_id": h.SessionID,
		"tool_name":  h.ToolName,
		"tool_input": h.ToolInput,
	}, 750*time.Millisecond)
}

// handlePermissionRequestRetry settles a prompt for a call Pilot already
// decided after a classifier denial. Returns handled=false when the call is
// new and the normal evaluation should run.
func handlePermissionRequestRetry(cfg *config.PilotConfig, h hookInput) (bool, error) {
	rec := claimRetry(cfg, h, retryStagePermissionRequest)
	if rec == nil {
		return false, nil
	}
	summary := toolSummary(h.ToolName, h.ToolInput)
	if rec.Decision == retryAllow {
		state.WriteLog("info", "approve", fmt.Sprintf("%s — allowing retry Pilot approved after classifier denial", summary))
		return true, printPermissionDecision("allow", "")
	}
	// Stand aside so Claude Code shows its own prompt: nobody answered within
	// the escalation window the first time, or Pilot could not evaluate.
	state.WriteLog("info", "approve", fmt.Sprintf("%s — leaving retry to Claude's prompt (%s)", summary, rec.Reason))
	return true, nil
}

// runPreToolUseClaim applies a decision Pilot registered after the auto-mode
// classifier denied this same call. "allow" is honoured by Claude Code ahead
// of the classifier, so the retry runs; "ask" forces Claude Code's prompt,
// which then reaches the PermissionRequest hook. Anything else: no output, so
// Claude Code's own permission flow (rules, classifier) proceeds untouched.
func runPreToolUseClaim(cfg *config.PilotConfig, h hookInput) error {
	rec := claimRetry(cfg, h, retryStagePreToolUse)
	if rec == nil {
		return nil
	}
	switch rec.Decision {
	case retryAllow:
		return printPreToolUseDecision("allow", "pilot: approved after auto-mode classifier denial — "+rec.Reason)
	case retryAsk:
		return printPreToolUseDecision("ask", "pilot: auto-mode classifier blocked this; confirm to run it — "+rec.Reason)
	}
	return nil
}

// handlePermissionDeniedResult runs after Claude Code's auto-mode classifier
// denied a call and Pilot's hierarchy has evaluated it. Claude Code cannot
// reverse the denial, but it lets a hook tell the model to retry. Pilot
// records what the retry should get and asks for one whenever it has a
// decision the classifier lacked: an approval (its own, or a human's) or a
// prompt for the human when nobody answered in time. A human rejection, or a
// deny rule in the user's settings, leaves the denial standing.
func handlePermissionDeniedResult(cfg *config.PilotConfig, h hookInput, result *evalResult, cliStart time.Time) error {
	roundTripMs := float64(time.Since(cliStart).Microseconds()) / 1000.0
	now := time.Now().UTC()
	summary := toolSummary(h.ToolName, h.ToolInput)
	classifierReason := h.Reason
	if classifierReason == "" {
		classifierReason = "denied by auto mode"
	}

	retry := func(decision, reason string) error {
		if decision == retryAsk && isHeadlessSession() {
			// `claude -p` has no prompt; the retry would be denied a second
			// time. Leave the classifier's denial standing instead.
			state.WriteLog("info", "on-denied", fmt.Sprintf("%s — headless session, no prompt to route the retry to; classifier denial stands (%s)", summary, reason))
			return nil
		}
		if !registerRetry(cfg, h, decision, reason) {
			state.WriteLog("warn", "on-denied", fmt.Sprintf("%s — could not register retry; classifier denial stands", summary))
			return nil
		}
		return printJSON(map[string]any{
			"hookSpecificOutput": map[string]any{
				"hookEventName": eventPermissionDenied,
				"retry":         true,
			},
		})
	}

	switch result.Decision {
	case "approve", "passthrough":
		confidence := 1.0
		_ = state.RecordAction(state.PilotAction{
			Timestamp:  now,
			ActionType: state.AutoApprove,
			Detail:     fmt.Sprintf("%s: %s [after auto-mode classifier: %s]", h.ToolName, result.Reason, classifierReason),
			Confidence: &confidence,
			DurationMs: &roundTripMs,
			Source:     result.Source,
		})
		state.WriteLog("info", "on-denied", fmt.Sprintf("%s — classifier said %q; Pilot approved (%s), asking model to retry", summary, classifierReason, result.Source))
		return retry(retryAllow, result.Reason)

	case "ask":
		// Evaluator unavailable, spend cap, or uninspectable input: the human
		// decides, through Claude's own prompt on the retry.
		state.WriteLog("warn", "on-denied", fmt.Sprintf("%s — classifier said %q; Pilot cannot decide (%s), routing retry to the user's prompt", summary, classifierReason, result.Reason))
		return retry(retryAsk, result.Reason)
	}

	// Pilot says deny. A settings deny rule is the user's own standing order;
	// everything else escalates to the human exactly as PermissionRequest does.
	confidence := 0.0
	if result.Source == "claude_settings" {
		_ = state.RecordAction(state.PilotAction{
			Timestamp:  now,
			ActionType: state.Escalate,
			Detail:     fmt.Sprintf("%s — %s [after auto-mode classifier: %s; denial stands]", summary, result.Reason, classifierReason),
			Confidence: &confidence,
			DurationMs: &roundTripMs,
			Source:     result.Source,
		})
		return nil
	}

	outcome := requestDashboardDecision(cfg, h.ToolName, h.ToolInput, result.Reason, result.Source, confidence, cfg.General.EscalationTimeoutS)
	switch outcome {
	case "human_approved":
		confidence = 1.0
		_ = state.RecordAction(state.PilotAction{
			Timestamp:  now,
			ActionType: state.AutoApprove,
			Detail:     fmt.Sprintf("%s — %s [dashboard, after auto-mode classifier: %s]", summary, result.Reason, classifierReason),
			Confidence: &confidence,
			DurationMs: &roundTripMs,
			Source:     result.Source,
		})
		state.WriteLog("info", "on-denied", fmt.Sprintf("%s — human approved on dashboard, asking model to retry", summary))
		return retry(retryAllow, "approved on the Pilot dashboard: "+result.Reason)
	case "human_rejected":
		_ = state.RecordAction(state.PilotAction{
			Timestamp:  now,
			ActionType: state.Escalate,
			Detail:     fmt.Sprintf("%s — rejected by human: %s [after auto-mode classifier: %s]", summary, result.Reason, classifierReason),
			Confidence: &confidence,
			DurationMs: &roundTripMs,
			Source:     result.Source,
		})
		return nil
	}

	// Timeout or dashboard error: same fallback as PermissionRequest — the
	// user's own prompt — reached by asking the model to retry once.
	_ = state.RecordAction(state.PilotAction{
		Timestamp:  now,
		ActionType: state.Escalate,
		Detail:     fmt.Sprintf("%s — %s [%s, after auto-mode classifier: %s]", summary, result.Reason, outcome, classifierReason),
		Confidence: &confidence,
		DurationMs: &roundTripMs,
		Source:     result.Source,
	})
	state.WriteLog("info", "on-denied", fmt.Sprintf("%s — escalation %s; routing retry to the user's prompt", summary, outcome))
	return retry(retryAsk, result.Reason)
}

func handleCodexEvalResult(cfg *config.PilotConfig, hookEventName string, result *evalResult, toolName, toolInput, cwd, sessionID string, cliStart time.Time) error {
	if strings.EqualFold(hookEventName, eventPermissionRequest) {
		return handlePermissionRequestResult(cfg, result, toolName, toolInput, cwd, sessionID, cliStart)
	}
	return handleCodexPreToolUseResult(cfg, result, toolName, toolInput, cwd, sessionID, cliStart)
}

func handleCodexPreToolUseResult(cfg *config.PilotConfig, result *evalResult, toolName, toolInput, cwd, sessionID string, cliStart time.Time) error {
	roundTripMs := float64(time.Since(cliStart).Microseconds()) / 1000.0
	if result.Decision == "approve" || result.Decision == "passthrough" || result.Decision == "ask" {
		return nil
	}

	now := time.Now().UTC()
	confidence := 0.0
	outcome := requestDashboardDecision(cfg, toolName, toolInput, result.Reason, result.Source, confidence, cfg.General.EscalationTimeoutS)
	if outcome == "human_approved" {
		confidence = 1.0
		_ = state.RecordAction(state.PilotAction{
			Timestamp:  now,
			ActionType: state.AutoApprove,
			Detail:     fmt.Sprintf("%s — approved by human before Codex tool use", toolSummary(toolName, toolInput)),
			Confidence: &confidence,
			DurationMs: &roundTripMs,
			Source:     result.Source,
		})
		return nil
	}

	detail := fmt.Sprintf("%s — flagged before Codex tool use: %s [%s]", toolSummary(toolName, toolInput), result.Reason, outcome)
	_ = state.RecordAction(state.PilotAction{
		Timestamp:  now,
		ActionType: state.Escalate,
		Detail:     detail,
		Confidence: &confidence,
		DurationMs: &roundTripMs,
		Source:     result.Source,
	})

	if outcome == "human_rejected" {
		return printCodexPreToolUseBlock("pilot: human rejected this tool use")
	}

	// PreToolUse cannot ask. On timeout, fail open and let Codex continue or
	// reach its normal PermissionRequest flow if the tool needs approval.
	return nil
}

func handlePermissionRequestResult(cfg *config.PilotConfig, result *evalResult, toolName, toolInput, cwd, sessionID string, cliStart time.Time) error {
	roundTripMs := float64(time.Since(cliStart).Microseconds()) / 1000.0

	if result.Decision == "ask" {
		return nil
	}

	if result.Decision == "approve" || result.Decision == "passthrough" {
		confidence := 1.0
		_ = state.RecordAction(state.PilotAction{
			Timestamp:  time.Now().UTC(),
			ActionType: state.AutoApprove,
			Detail:     fmt.Sprintf("%s: %s", toolName, result.Reason),
			Confidence: &confidence,
			DurationMs: &roundTripMs,
			Source:     result.Source,
		})
		return printPermissionDecision("allow", "")
	}

	confidence := 0.0
	outcome := requestDashboardDecision(cfg, toolName, toolInput, result.Reason, result.Source, confidence, cfg.General.EscalationTimeoutS)
	now := time.Now().UTC()
	if outcome == "human_approved" {
		confidence = 1.0
		_ = state.RecordAction(state.PilotAction{
			Timestamp:  now,
			ActionType: state.AutoApprove,
			Detail:     fmt.Sprintf("%s — %s [dashboard]", toolSummary(toolName, toolInput), result.Reason),
			Confidence: &confidence,
			DurationMs: &roundTripMs,
			Source:     result.Source,
		})
		return printPermissionDecision("allow", "")
	}
	if outcome == "human_rejected" {
		_ = state.RecordAction(state.PilotAction{
			Timestamp:  now,
			ActionType: state.Escalate,
			Detail:     fmt.Sprintf("%s — rejected by human: %s", toolSummary(toolName, toolInput), result.Reason),
			Confidence: &confidence,
			DurationMs: &roundTripMs,
			Source:     result.Source,
		})
		return printPermissionDecision("deny", "pilot: human rejected this approval request")
	}

	// Let the runtime show its normal approval prompt on timeout or dashboard errors.
	_ = state.RecordAction(state.PilotAction{
		Timestamp:  now,
		ActionType: state.Escalate,
		Detail:     fmt.Sprintf("%s — %s [%s]", toolSummary(toolName, toolInput), result.Reason, outcome),
		Confidence: &confidence,
		DurationMs: &roundTripMs,
		Source:     result.Source,
	})
	return nil
}

func printCodexPreToolUseBlock(reason string) error {
	return printPreToolUseDecision("deny", reason)
}

func printPreToolUseDecision(behavior, reason string) error {
	return printJSON(map[string]any{
		"hookSpecificOutput": map[string]any{
			"hookEventName":            eventPreToolUse,
			"permissionDecision":       behavior,
			"permissionDecisionReason": reason,
		},
	})
}

func printPermissionDecision(behavior, message string) error {
	decision := map[string]any{"behavior": behavior}
	if message != "" {
		decision["message"] = message
	}
	return printJSON(map[string]any{
		"hookSpecificOutput": map[string]any{
			"hookEventName": eventPermissionRequest,
			"decision":      decision,
		},
	})
}

// emitActionToSSE sends an action event to the SSE server (fire-and-forget).
func emitActionToSSE(cfg *config.PilotConfig, ts time.Time, actionType, detail string, confidence *float64, toolName, toolInput, cwd, sessionID string) {
	body, _ := json.Marshal(map[string]any{
		"timestamp":   ts.Format(time.RFC3339Nano),
		"action_type": actionType,
		"detail":      detail,
		"confidence":  confidence,
		"tool_name":   toolName,
		"tool_input":  toolInput,
		"cwd":         cwd,
		"session_id":  sessionID,
	})

	resp, err := postServe(cfg, "/internal/action", body, 500*time.Millisecond)
	if err != nil {
		slog.Debug("SSE server not reachable", "error", err)
		return
	}
	resp.Body.Close()
}

// requestDashboardDecision sends a pending approval to the dashboard and blocks
// for timeoutS seconds waiting for approve/reject. Returns "approved", "rejected", or "timeout".
// Falls back to "timeout" if server is unreachable.
func requestDashboardDecision(cfg *config.PilotConfig, toolName, toolInput, reason, source string, confidence float64, timeoutS float64) string {
	if timeoutS <= 0 {
		return "timeout"
	}

	body, _ := json.Marshal(map[string]any{
		"tool_name":      toolName,
		"source":         source,
		"tool_input":     toolInput,
		"reason":         reason,
		"confidence":     confidence,
		"grace_period_s": timeoutS,
	})

	timeout := time.Duration(timeoutS*float64(time.Second)) + 2*time.Second
	resp, err := postServe(cfg, "/internal/pending", body, timeout)
	if err != nil {
		slog.Debug("SSE server not reachable for pending decision", "error", err)
		return "timeout"
	}
	defer resp.Body.Close()

	var result struct {
		Outcome    string `json:"outcome"`
		ResolvedBy string `json:"resolved_by"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "timeout"
	}
	if result.ResolvedBy == "human" {
		return "human_" + result.Outcome // "human_approved" or "human_rejected"
	}
	return result.Outcome
}

// toolSummary returns a short human-readable summary of the tool call.
// e.g. "Bash: railway up -d ..." or "Edit: /path/to/file.go"
func toolSummary(toolName, toolInput string) string {
	if toolInput == "" {
		return toolName
	}
	var parsed map[string]any
	if err := json.Unmarshal([]byte(toolInput), &parsed); err == nil {
		switch toolName {
		case "Bash":
			if cmd, ok := parsed["command"].(string); ok {
				if len(cmd) > 80 {
					cmd = cmd[:80] + "..."
				}
				return toolName + ": " + cmd
			}
		case "apply_patch":
			if cmd, ok := parsed["command"].(string); ok {
				if len(cmd) > 80 {
					cmd = cmd[:80] + "..."
				}
				return toolName + ": " + cmd
			}
		case "Edit", "Write", "NotebookEdit", "Read":
			if fp, ok := parsed["file_path"].(string); ok {
				return toolName + ": " + fp
			}
		case "Grep":
			p, _ := parsed["pattern"].(string)
			path, _ := parsed["path"].(string)
			if p != "" {
				summary := toolName + ": " + p
				if path != "" {
					summary += " in " + path
				}
				return summary
			}
		case "Glob":
			pat, _ := parsed["pattern"].(string)
			path, _ := parsed["path"].(string)
			if pat != "" {
				summary := toolName + ": " + pat
				if path != "" {
					summary += " in " + path
				}
				return summary
			}
		case "Agent":
			desc, _ := parsed["description"].(string)
			if desc != "" {
				return toolName + ": " + desc
			}
		case "WebFetch":
			if url, ok := parsed["url"].(string); ok {
				return toolName + ": " + url
			}
		}
	}
	if len(toolInput) > 80 {
		return toolName + ": " + toolInput[:80] + "..."
	}
	return toolName + ": " + toolInput
}

func printJSON(v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	fmt.Println(string(data))
	return nil
}
