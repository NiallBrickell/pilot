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

func init() {
	rootCmd.AddCommand(&cobra.Command{
		Use:   "approve",
		Short: "Run as a Claude Code PreToolUse hook (reads tool info from stdin, returns approve/deny)",
		RunE:  runApprove,
	})
}

func runApprove(cmd *cobra.Command, args []string) error {
	paths.EnsureSetup(config.EmbeddedConfig())
	if !auth.IsClaudeAuthed() {
		reason := "pilot: claude not authenticated, skipping"
		return printJSON(hookResponse{
			HookSpecificOutput: preToolUseOutput{
				HookEventName:            "PreToolUse",
				PermissionDecision:       "ask",
				PermissionDecisionReason: &reason,
			},
		})
	}

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
	transcriptPath, _ := toolInfo["transcript_path"].(string)

	// Hash of last user message text to detect new user turns for interrogation
	var userMsgHash string
	if transcriptPath != "" {
		if data, err := os.ReadFile(transcriptPath); err == nil {
			lines := strings.Split(strings.TrimSpace(string(data)), "\n")
			for i := len(lines) - 1; i >= 0; i-- {
				var entry map[string]any
				if json.Unmarshal([]byte(lines[i]), &entry) != nil {
					continue
				}
				if msg, ok := entry["message"].(map[string]any); ok {
					if role, _ := msg["role"].(string); role == "user" {
						// Hash the actual message content, not the JSONL line
						switch content := msg["content"].(type) {
						case string:
							userMsgHash = content[:min(len(content), 200)]
						case []any:
							for _, item := range content {
								if m, ok := item.(map[string]any); ok {
									if t, ok := m["text"].(string); ok {
										userMsgHash = t[:min(len(t), 200)]
										break
									}
								}
							}
						}
						break
					}
				}
			}
		}
	}

	cfg := config.Load()

	// Evaluate via serve. If serve isn't running, fail fast — don't hang.
	result, ok := evaluateViaServer(cfg, toolName, toolInput, sessionCwd, sessionID, transcriptPath, userMsgHash)
	if !ok {
		// Serve not running. Let Claude prompt the user normally.
		reason := "pilot: serve not running — run 'pilot serve' or toggle pilot on in dashboard"
		slog.Warn(reason)
		return printJSON(hookResponse{
			HookSpecificOutput: preToolUseOutput{
				HookEventName:            "PreToolUse",
				PermissionDecision:       "ask",
				PermissionDecisionReason: &reason,
			},
		})
	}

	return handleEvalResult(cfg, result, toolName, toolInput, sessionCwd, sessionID)
}

type evalResult struct {
	Decision   string  `json:"decision"`
	Reason     string  `json:"reason"`
	Confidence float64 `json:"confidence"`
	Source     string  `json:"source"` // "settings", "pilot", "haiku", "interrogate"
}

// evaluateViaServer calls the pilot serve process to evaluate.
func evaluateViaServer(cfg *config.PilotConfig, toolName, toolInput, cwd, sessionID, transcriptPath, userMsgHash string) (*evalResult, bool) {
	body, _ := json.Marshal(map[string]any{
		"tool_name":       toolName,
		"tool_input":      toolInput,
		"cwd":             cwd,
		"session_id":      sessionID,
		"transcript_path": transcriptPath,
		"user_msg_hash":   userMsgHash,
	})

	client := &http.Client{Timeout: 30 * time.Second}
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
func handleEvalResult(cfg *config.PilotConfig, result *evalResult, toolName, toolInput, cwd, sessionID string) error {
	if result.Decision == "passthrough" {
		// Emit a "settings passthrough" event so dashboard can show it
		emitActionToSSE(cfg, time.Now().UTC(), "passthrough", fmt.Sprintf("%s: %s", toolName, result.Reason), nil, toolName, toolInput, cwd, sessionID)
		return printJSON(hookResponse{
			HookSpecificOutput: preToolUseOutput{
				HookEventName: "PreToolUse",
			},
		})
	}

	now := time.Now().UTC()

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

		_ = state.RecordAction(state.PilotAction{
			Timestamp:  now,
			ActionType: state.AutoApprove,
			Detail:     fmt.Sprintf("%s: %s", toolName, result.Reason),
			Confidence: &confidence,
		})

		return printJSON(hookResponse{
			HookSpecificOutput: preToolUseOutput{
				HookEventName:            "PreToolUse",
				PermissionDecision:       "allow",
				PermissionDecisionReason: &result.Reason,
			},
		})
	}

	// Interrogation: pilot detected Claude may be off-track.
	// High confidence → deny directly, Claude course-corrects.
	// Lower confidence → escalate to dashboard for human decision.
	if result.Source == "interrogate" {
		if result.Confidence >= 0.85 {
			confidence := result.Confidence
			_ = state.RecordAction(state.PilotAction{
				Timestamp:  time.Now().UTC(),
				ActionType: "interrogate",
				Detail:     fmt.Sprintf("%s: %s", toolName, result.Reason),
				Confidence: &confidence,
			})
			return printJSON(hookResponse{
				HookSpecificOutput: preToolUseOutput{
					HookEventName:            "PreToolUse",
					PermissionDecision:       "deny",
					PermissionDecisionReason: &result.Reason,
				},
			})
		}
		// Lower confidence — fall through to escalation below
	}

	// Escalation: haiku says deny (or low-confidence interrogation).
	// Send to dashboard for human decision.
	confidence := 0.0
	outcome := requestDashboardDecision(cfg, toolName, toolInput, result.Reason, result.Source, confidence, cfg.General.EscalationTimeoutS)
	if outcome == "approved" {
		// Human approved from dashboard
		confidence = 1.0
		reason := "approved from dashboard"
		_ = state.RecordAction(state.PilotAction{
			Timestamp:  now,
			ActionType: state.AutoApprove,
			Detail:     fmt.Sprintf("%s: %s", toolName, reason),
			Confidence: &confidence,
		})
		return printJSON(hookResponse{
			HookSpecificOutput: preToolUseOutput{
				HookEventName:            "PreToolUse",
				PermissionDecision:       "allow",
				PermissionDecisionReason: &reason,
			},
		})
	}

	// Timed out or rejected — fall through to Claude's normal prompt
	_ = state.RecordAction(state.PilotAction{
		Timestamp:  now,
		ActionType: state.Escalate,
		Detail:     fmt.Sprintf("%s: %s", toolName, result.Reason),
		Confidence: &confidence,
	})

	return printJSON(hookResponse{
		HookSpecificOutput: preToolUseOutput{
			HookEventName:            "PreToolUse",
			PermissionDecision:       "ask",
			PermissionDecisionReason: &result.Reason,
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
		slog.Debug("SSE server not reachable for grace period, auto-approving", "error", err)
		return "approved"
	}
	defer resp.Body.Close()

	var result struct {
		Outcome string `json:"outcome"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "approved"
	}
	return result.Outcome
}

func printJSON(v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	fmt.Println(string(data))
	return nil
}
