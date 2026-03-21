// Layer 2: Pilot's own fast rules.
// Pattern-based rules configured in pilot.toml that don't need an LLM call.
// These catch common cases that Claude settings don't cover.
package approve

import (
	"github.com/NiallBrickell/pilot/internal/config"
)

// CheckPilotRules evaluates against pilot's own rule set.
// Returns "approve", "deny", or "" (no match, fall through to LLM).
func CheckPilotRules(cfg *config.PilotConfig, toolName, toolInput string) string {
	// Currently pilot doesn't have its own rule set beyond what's in the
	// approval prompt. This is the extension point for adding fast rules
	// without LLM calls, e.g. from a [rules] section in pilot.toml.
	//
	// For now, return empty to fall through to haiku.
	return ""
}
