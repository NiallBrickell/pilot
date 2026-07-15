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

	"github.com/NiallBrickell/pilot/internal/auth"
	"github.com/NiallBrickell/pilot/internal/config"
	"github.com/NiallBrickell/pilot/internal/paths"
	"github.com/NiallBrickell/pilot/internal/state"

	"github.com/spf13/cobra"
)

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

func init() {
	rootCmd.AddCommand(&cobra.Command{
		Use:   "approve",
		Short: "Run as a Claude Code PreToolUse hook (reads tool info from stdin, returns approve/deny)",
		RunE:  func(cmd *cobra.Command, args []string) error { return runApproveForRuntime(runtimeClaude) },
	})
	rootCmd.AddCommand(&cobra.Command{
		Use:   "codex-approve",
		Short: "Run as a Codex PreToolUse or PermissionRequest hook",
		RunE:  func(cmd *cobra.Command, args []string) error { return runApproveForRuntime(runtimeCodex) },
	})
}

func runApproveForRuntime(runtime hookRuntime) error {
	cliStart := time.Now()
	paths.EnsureSetup(config.EmbeddedConfig())

	input, err := io.ReadAll(os.Stdin)
	if err != nil {
		return err
	}

	slog.Debug("Hook input", "input", string(input))

	var toolInfo map[string]any
	if err := json.Unmarshal(input, &toolInfo); err != nil {
		toolInfo = map[string]any{}
	}

	toolName, _ := toolInfo["tool_name"].(string)
	if toolName == "" {
		toolName = "unknown"
	}
	hookEventName, _ := toolInfo["hook_event_name"].(string)
	if hookEventName == "" {
		hookEventName, _ = toolInfo["hookEventName"].(string)
	}

	if runtime == runtimeCodex {
		state.WriteLog("debug", "codex-approve", fmt.Sprintf("tool=%s hookEvent=%q cwd=%q", toolName, hookEventName, toolInfo["cwd"]))
	}

	var toolInput string
	switch v := toolInfo["tool_input"].(type) {
	case string:
		toolInput = v
	case map[string]any, []any:
		b, _ := json.Marshal(v)
		toolInput = string(b)
	default:
		toolInput = ""
	}

	sessionCwd, _ := toolInfo["cwd"].(string)
	if sessionCwd == "" {
		sessionCwd, _ = os.Getwd()
	}
	sessionID, _ := toolInfo["session_id"].(string)
	if sessionID == "" {
		sessionID, _ = toolInfo["turn_id"].(string)
	}
	transcriptPath, _ := toolInfo["transcript_path"].(string)

	// Hash of last user message text to detect new user turns for interrogation.
	// Only read the tail of the transcript to avoid loading huge files into memory.
	var userMsgHash string
	if transcriptPath != "" {
		userMsgHash = lastUserMsgHash(transcriptPath)
	}

	if runtime == runtimeClaude && !auth.IsClaudeAuthed() {
		reason := "pilot: claude not authenticated, skipping"
		return printJSON(hookResponse{
			HookSpecificOutput: preToolUseOutput{
				HookEventName:            "PreToolUse",
				PermissionDecision:       "ask",
				PermissionDecisionReason: &reason,
			},
		})
	}

	cfg := config.Load()

	// Evaluate via serve. If serve isn't running, fail open — pilot is
	// effectively off, so the hook should be a silent no-op rather than
	// forcing the user to approve every command.
	result, ok := evaluateViaServer(cfg, runtime, toolName, toolInput, sessionCwd, sessionID, transcriptPath, userMsgHash)
	if !ok {
		if runtime == runtimeCodex {
			slog.Debug("pilot: serve not running, leaving Codex hook undecided")
			return nil
		}
		reason := "pilot: serve not running, allowing"
		slog.Debug(reason)
		return printJSON(hookResponse{
			HookSpecificOutput: preToolUseOutput{
				HookEventName:            "PreToolUse",
				PermissionDecision:       "allow",
				PermissionDecisionReason: &reason,
			},
		})
	}

	return handleEvalResult(cfg, runtime, hookEventName, result, toolName, toolInput, sessionCwd, sessionID, cliStart)
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
	client := &http.Client{Timeout: 45 * time.Second}
	resp, err := client.Post(config.SSEBaseURL(cfg)+"/internal/evaluate", "application/json", bytes.NewReader(body))
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
func handleEvalResult(cfg *config.PilotConfig, runtime hookRuntime, hookEventName string, result *evalResult, toolName, toolInput, cwd, sessionID string, cliStart time.Time) error {
	if runtime == runtimeCodex {
		return handleCodexEvalResult(cfg, hookEventName, result, toolName, toolInput, cwd, sessionID, cliStart)
	}

	roundTripMs := float64(time.Since(cliStart).Microseconds()) / 1000.0

	if result.Decision == "passthrough" {
		// Emit a "settings passthrough" event so dashboard can show it
		emitActionToSSE(cfg, time.Now().UTC(), "passthrough", fmt.Sprintf("%s: %s", toolName, result.Reason), nil, toolName, toolInput, cwd, sessionID)
		slog.Debug("Approve complete", "tool", toolName, "decision", "passthrough", "source", result.Source, "server_ms", result.DurationMs, "roundtrip_ms", roundTripMs)
		reason := "pilot: auto-approved by settings"
		return printJSON(hookResponse{
			HookSpecificOutput: preToolUseOutput{
				HookEventName:            "PreToolUse",
				PermissionDecision:       "allow",
				PermissionDecisionReason: &reason,
			},
		})
	}

	now := time.Now().UTC()

	// Evaluator down or other infrastructure issue — fall through to user
	if result.Decision == "ask" {
		slog.Warn("Pilot infrastructure issue, falling through to user", "reason", result.Reason, "roundtrip_ms", roundTripMs)
		return printJSON(hookResponse{
			HookSpecificOutput: preToolUseOutput{
				HookEventName:            "PreToolUse",
				PermissionDecision:       "ask",
				PermissionDecisionReason: &result.Reason,
			},
		})
	}

	if result.Decision == "approve" {
		confidence := 1.0

		// Grace period for approvals
		if cfg.General.GracePeriodS > 0 {
			outcome := requestDashboardDecision(cfg, toolName, toolInput, result.Reason, "haiku", confidence, cfg.General.GracePeriodS)
			if outcome == "rejected" {
				reason := "pilot: human rejected during grace period"
				_ = state.RecordAction(state.PilotAction{
					Timestamp:  now,
					ActionType: state.Escalate,
					Detail:     fmt.Sprintf("%s: rejected by human during grace period", toolName),
					Confidence: &confidence,
					DurationMs: &roundTripMs,
					Source:     result.Source,
				})
				return printJSON(hookResponse{
					HookSpecificOutput: preToolUseOutput{
						HookEventName:            "PreToolUse",
						PermissionDecision:       "ask",
						PermissionDecisionReason: &reason,
					},
				})
			}
		}

		slog.Debug("Approve complete", "tool", toolName, "decision", "approve", "source", result.Source, "server_ms", result.DurationMs, "roundtrip_ms", roundTripMs)

		_ = state.RecordAction(state.PilotAction{
			Timestamp:  now,
			ActionType: state.AutoApprove,
			Detail:     fmt.Sprintf("%s: %s", toolName, result.Reason),
			Confidence: &confidence,
			DurationMs: &roundTripMs,
			Source:     result.Source,
		})

		return printJSON(hookResponse{
			HookSpecificOutput: preToolUseOutput{
				HookEventName:            "PreToolUse",
				PermissionDecision:       "allow",
				PermissionDecisionReason: &result.Reason,
			},
		})
	}

	// Escalation: haiku says deny.
	// Send to dashboard for human decision.
	confidence := 0.0
	outcome := requestDashboardDecision(cfg, toolName, toolInput, result.Reason, result.Source, confidence, cfg.General.EscalationTimeoutS)
	if outcome == "human_approved" {
		// Human explicitly approved from dashboard
		confidence = 1.0
		detail := fmt.Sprintf("%s — %s [dashboard]", toolSummary(toolName, toolInput), result.Reason)
		_ = state.RecordAction(state.PilotAction{
			Timestamp:  now,
			ActionType: state.AutoApprove,
			Detail:     detail,
			Confidence: &confidence,
			DurationMs: &roundTripMs,
			Source:     result.Source,
		})
		reason := fmt.Sprintf("approved (dashboard): %s", result.Reason)
		return printJSON(hookResponse{
			HookSpecificOutput: preToolUseOutput{
				HookEventName:            "PreToolUse",
				PermissionDecision:       "allow",
				PermissionDecisionReason: &reason,
			},
		})
	}

	// Timeout, rejected, or error — fall through to Claude's normal prompt
	detail := fmt.Sprintf("%s — %s [%s]", toolSummary(toolName, toolInput), result.Reason, outcome)
	_ = state.RecordAction(state.PilotAction{
		Timestamp:  now,
		ActionType: state.Escalate,
		Detail:     detail,
		Confidence: &confidence,
		DurationMs: &roundTripMs,
		Source:     result.Source,
	})

	return printJSON(hookResponse{
		HookSpecificOutput: preToolUseOutput{
			HookEventName:            "PreToolUse",
			PermissionDecision:       "ask",
			PermissionDecisionReason: &result.Reason,
		},
	})
}

func handleCodexEvalResult(cfg *config.PilotConfig, hookEventName string, result *evalResult, toolName, toolInput, cwd, sessionID string, cliStart time.Time) error {
	if strings.EqualFold(hookEventName, "PermissionRequest") {
		return handleCodexPermissionRequestResult(cfg, result, toolName, toolInput, cwd, sessionID, cliStart)
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

func handleCodexPermissionRequestResult(cfg *config.PilotConfig, result *evalResult, toolName, toolInput, cwd, sessionID string, cliStart time.Time) error {
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
		return printCodexPermissionDecision("allow", "")
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
		return printCodexPermissionDecision("allow", "")
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
		return printCodexPermissionDecision("deny", "pilot: human rejected this approval request")
	}

	// Let Codex show its normal approval prompt on timeout or dashboard errors.
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
	return printJSON(map[string]any{
		"hookSpecificOutput": map[string]any{
			"hookEventName":            "PreToolUse",
			"permissionDecision":       "deny",
			"permissionDecisionReason": reason,
		},
	})
}

func printCodexPermissionDecision(behavior, message string) error {
	decision := map[string]any{"behavior": behavior}
	if message != "" {
		decision["message"] = message
	}
	return printJSON(map[string]any{
		"hookSpecificOutput": map[string]any{
			"hookEventName": "PermissionRequest",
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

	client := &http.Client{Timeout: 500 * time.Millisecond}
	resp, err := client.Post(config.SSEBaseURL(cfg)+"/internal/action", "application/json", bytes.NewReader(body))
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
	client := &http.Client{Timeout: timeout}

	resp, err := client.Post(config.SSEBaseURL(cfg)+"/internal/pending", "application/json", bytes.NewReader(body))
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
