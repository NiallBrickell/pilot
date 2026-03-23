// Package approve implements the three-layer approval hierarchy:
//   1. Claude Code settings — user's own rules, fast, no LLM
//   2. Pilot rules — configurable pattern rules, no LLM
//   3. Haiku evaluation — LLM fallback for everything else
//
// Tool calls flow through in order. First match wins.
package approve

import (
	"github.com/NiallBrickell/pilot/internal/config"
)

type Decision struct {
	Action string // "passthrough", "approve", "deny"
	Reason string
	Source string // "claude_settings", "pilot_rules", "haiku"
}

// Evaluate runs the tool call through the approval hierarchy.
// Returns a Decision with the source that made it.
func Evaluate(cfg *config.PilotConfig, toolName, toolInput, cwd string) *Decision {
	// Layer 1: Claude Code settings
	if result := CheckClaudeSettings(toolName, toolInput, cwd); result != "" {
		action := "passthrough"
		if result == "deny" {
			action = "deny"
		}
		return &Decision{
			Action: action,
			Reason: "matched Claude Code settings",
			Source: "claude_settings",
		}
	}

	// Layer 2: Pilot rules
	if result := CheckPilotRules(cfg, toolName, toolInput, cwd); result != "" {
		return &Decision{
			Action: result,
			Reason: "matched pilot rule",
			Source: "pilot_rules",
		}
	}

	// Layer 3: falls through — caller should use haiku
	return nil
}
