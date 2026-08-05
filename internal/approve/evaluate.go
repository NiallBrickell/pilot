// Package approve implements the three-layer approval hierarchy:
//  1. Claude Code settings — user's own rules, fast, no LLM
//  2. Pilot rules — configurable pattern rules, no LLM
//  3. Haiku evaluation — LLM fallback for everything else
//
// Tool calls flow through in order. First match wins.
package approve

import (
	"encoding/json"

	"github.com/erdoai/pilot/internal/config"
)

type Decision struct {
	Action string // "passthrough", "approve", "deny"
	Reason string
	Source string // "claude_settings", "codex_settings", "pilot_rules", "haiku"
}

// Evaluate runs the tool call through the approval hierarchy.
// Returns a Decision with the source that made it.
func Evaluate(cfg *config.PilotConfig, toolName, toolInput, cwd string) *Decision {
	return EvaluateForRuntime(cfg, "claude", toolName, toolInput, cwd)
}

func EvaluateForRuntime(cfg *config.PilotConfig, runtime, toolName, toolInput, cwd string) *Decision {
	// Parse toolInput JSON once — reused by all layers.
	var parsed map[string]any
	if len(toolInput) > 0 && toolInput[0] == '{' {
		_ = json.Unmarshal([]byte(toolInput), &parsed)
	}

	if runtime == "" || runtime == "claude" {
		// Layer 1: Claude Code settings
		if result := CheckClaudeSettings(toolName, parsed, toolInput, cwd); result != "" {
			// An allow-list match must APPROVE, not passthrough: headless
			// callers of /internal/evaluate have no Claude Code permission
			// layer beneath them, so "passthrough" reads as a non-approval
			// and escalates to the human — inverting what the allow list
			// says. In the hook flow the outcomes are identical (the command
			// runs either way); "ask" matches keep passing through so the
			// interactive prompt (or a headless caller's escalation) still
			// reaches the human.
			action := "passthrough"
			switch result {
			case "deny":
				action = "deny"
			case "allow":
				action = "approve"
			}
			return &Decision{
				Action: action,
				Reason: "matched Claude Code settings",
				Source: "claude_settings",
			}
		}
	}

	if runtime == "codex" {
		// Layer 1: Codex local trust/config
		if result := CheckCodexSettings(toolName, parsed, toolInput, cwd); result != "" {
			action := "passthrough"
			if result == "deny" {
				action = "deny"
			}
			return &Decision{
				Action: action,
				Reason: "matched Codex settings",
				Source: "codex_settings",
			}
		}
	}

	// Layer 2: Pilot rules
	if result := CheckPilotRules(cfg, toolName, parsed, cwd); result != "" {
		return &Decision{
			Action: result,
			Reason: "matched pilot rule",
			Source: "pilot_rules",
		}
	}

	// Layer 3: falls through — caller should use haiku
	return nil
}
