// Layer 2: Pilot's own fast rules.
// Pattern-based rules that don't need an LLM call.
package approve

import (
	"encoding/json"
	"path/filepath"
	"strings"

	"github.com/NiallBrickell/pilot/internal/config"
)

// readOnlyTools are tools that only observe — never mutate state.
var readOnlyTools = map[string]bool{
	"Read": true, "Grep": true, "Glob": true,
}

// CheckPilotRules evaluates against pilot's own rule set.
// Returns "approve", "deny", or "" (no match, fall through to LLM).
func CheckPilotRules(cfg *config.PilotConfig, toolName, toolInput, cwd string) string {
	if !readOnlyTools[toolName] {
		return "" // Not a read-only tool — fall through
	}

	// Auto-approve read-only tools that target the working directory.
	// Out-of-cwd reads fall through to LLM evaluation.
	target := extractReadTarget(toolName, toolInput)
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

// extractReadTarget pulls the file/directory path from a read-only tool's input.
func extractReadTarget(toolName, toolInput string) string {
	if toolInput == "" || toolInput[0] != '{' {
		return ""
	}
	var parsed map[string]any
	if err := json.Unmarshal([]byte(toolInput), &parsed); err != nil {
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
