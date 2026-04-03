// Layer 2: Pilot's own fast rules.
// Pattern-based rules that don't need an LLM call.
package approve

import (
	"path/filepath"
	"strings"

	"github.com/NiallBrickell/pilot/internal/config"
)

// readOnlyTools are tools that only observe — never mutate state.
var readOnlyTools = map[string]bool{
	"Read": true, "Grep": true, "Glob": true,
}

// CheckPilotRules evaluates against pilot's own rule set.
// parsed is the pre-parsed toolInput JSON (nil if not JSON).
// Returns "approve", "deny", or "" (no match, fall through to LLM).
func CheckPilotRules(cfg *config.PilotConfig, toolName string, parsed map[string]any, cwd string) string {
	if !readOnlyTools[toolName] {
		return "" // Not a read-only tool — fall through
	}

	// Auto-approve read-only tools that target the working directory.
	// Out-of-cwd reads fall through to LLM evaluation.
	target := extractReadTarget(toolName, parsed)
	if target == "" {
		return "approve" // No path to check (e.g. Grep with no explicit path) — approve
	}

	abs, err := filepath.Abs(target)
	if err != nil {
		return "approve"
	}

	if cwd != "" {
		absCwd, err := filepath.Abs(cwd)
		if err == nil && isWithinDir(abs, absCwd) {
			return "approve"
		}
	}

	// Out-of-cwd read — fall through to LLM evaluation
	return ""
}

// extractReadTarget pulls the file/directory path from pre-parsed read-only tool input.
func extractReadTarget(toolName string, parsed map[string]any) string {
	if parsed == nil {
		return ""
	}

	switch toolName {
	case "Read":
		if fp, ok := parsed["file_path"].(string); ok {
			return fp
		}
	case "Grep":
		if p, ok := parsed["path"].(string); ok {
			return p
		}
	case "Glob":
		if p, ok := parsed["path"].(string); ok {
			return p
		}
	}
	return ""
}

// isWithinDir checks if target path is within or equal to root.
func isWithinDir(target, root string) bool {
	rel, err := filepath.Rel(root, target)
	if err != nil {
		return false
	}
	return !strings.HasPrefix(rel, "..")
}
