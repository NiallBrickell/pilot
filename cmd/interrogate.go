package cmd

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/NiallBrickell/pilot/internal/auth"
	"github.com/NiallBrickell/pilot/internal/config"
	"github.com/NiallBrickell/pilot/internal/paths"
	"github.com/NiallBrickell/pilot/internal/state"

	"github.com/spf13/cobra"
)

func init() {
	rootCmd.AddCommand(&cobra.Command{
		Use:   "interrogate",
		Short: "Run as a Claude Code PreToolUse hook for interrogation (is Claude on track?)",
		RunE:  func(cmd *cobra.Command, args []string) error { return runInterrogateForRuntime(runtimeClaude) },
	})
	rootCmd.AddCommand(&cobra.Command{
		Use:   "codex-interrogate",
		Short: "Run as a Codex PreToolUse hook for interrogation (is Codex on track?)",
		RunE:  func(cmd *cobra.Command, args []string) error { return runInterrogateForRuntime(runtimeCodex) },
	})
}

func runInterrogateForRuntime(runtime hookRuntime) error {
	paths.EnsureSetup(config.EmbeddedConfig())

	input, err := io.ReadAll(os.Stdin)
	if err != nil {
		return err
	}

	slog.Debug("Interrogate hook input", "input", string(input))

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
	if sessionID == "" {
		sessionID, _ = toolInfo["turn_id"].(string)
	}
	transcriptPath, _ := toolInfo["transcript_path"].(string)

	cfg := config.Load()
	if !cfg.General.IsInterrogationEnabled() {
		if runtime == runtimeCodex {
			return nil
		}
		return printJSON(hookResponse{
			HookSpecificOutput: preToolUseOutput{
				HookEventName:      "PreToolUse",
				PermissionDecision: "allow",
			},
		})
	}

	// Hash of last user message text to detect new user turns.
	// Only read the tail of the transcript to avoid loading huge files into memory.
	var userMsgHash string
	if transcriptPath != "" {
		userMsgHash = lastUserMsgHash(transcriptPath)
	}

	if runtime == runtimeClaude && !auth.IsClaudeAuthed() {
		return printJSON(hookResponse{
			HookSpecificOutput: preToolUseOutput{
				HookEventName:      "PreToolUse",
				PermissionDecision: "allow",
			},
		})
	}

	result, ok := interrogateViaServer(cfg, toolName, toolInput, sessionCwd, sessionID, transcriptPath, userMsgHash)
	if !ok {
		// Serve not running — allow through silently
		if runtime == runtimeCodex {
			return nil
		}
		return printJSON(hookResponse{
			HookSpecificOutput: preToolUseOutput{
				HookEventName:      "PreToolUse",
				PermissionDecision: "allow",
			},
		})
	}

	if result.Decision == "deny" && result.Source == "interrogate" {
		// Off-track: deny with redirect message
		confidence := 0.0
		detail := fmt.Sprintf("%s — REDIRECTED: %s", toolSummary(toolName, toolInput), result.Reason)
		_ = state.RecordAction(state.PilotAction{
			Timestamp:  time.Now().UTC(),
			ActionType: state.Escalate,
			Detail:     detail,
			Confidence: &confidence,
		})
		emitActionToSSE(cfg, time.Now().UTC(), "interrogate", detail, &confidence, toolName, toolInput, sessionCwd, sessionID)
		if runtime == runtimeCodex {
			return printCodexPreToolUseBlock(result.Reason)
		}
		return printJSON(hookResponse{
			HookSpecificOutput: preToolUseOutput{
				HookEventName:            "PreToolUse",
				PermissionDecision:       "ask",
				PermissionDecisionReason: &result.Reason,
			},
		})
	}

	// On track — allow
	if runtime == runtimeCodex {
		return nil
	}
	return printJSON(hookResponse{
		HookSpecificOutput: preToolUseOutput{
			HookEventName:      "PreToolUse",
			PermissionDecision: "allow",
		},
	})
}

func interrogateViaServer(cfg *config.PilotConfig, toolName, toolInput, cwd, sessionID, transcriptPath, userMsgHash string) (*evalResult, bool) {
	body, _ := json.Marshal(map[string]any{
		"tool_name":       toolName,
		"tool_input":      toolInput,
		"cwd":             cwd,
		"session_id":      sessionID,
		"transcript_path": transcriptPath,
		"user_msg_hash":   userMsgHash,
	})

	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Post(config.SSEBaseURL(cfg)+"/internal/interrogate", "application/json", bytes.NewReader(body))
	if err != nil {
		slog.Debug("Serve not reachable for interrogation", "error", err)
		return nil, false
	}
	defer resp.Body.Close()

	var result evalResult
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, false
	}
	return &result, true
}
