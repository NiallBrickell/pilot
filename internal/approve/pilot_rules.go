// Layer 2: Pilot's own fast rules.
// Pattern-based rules that don't need an LLM call.
package approve

import (
	"path/filepath"
	"regexp"
	"strings"

	"github.com/NiallBrickell/pilot/internal/config"
)

// readOnlyTools are tools that only observe — never mutate state.
var readOnlyTools = map[string]bool{
	"Read": true, "Grep": true, "Glob": true,
}

// bashTools are the shell-exec tools whose command we inspect for the
// deterministic safe-command allowlist.
var bashTools = map[string]bool{
	"Bash": true, "shell": true, "local_shell": true,
}

// safeBashCommandRe matches commands that are routine and safe but which the
// haiku evaluator intermittently over-denies by over-extrapolating from the
// deny list (e.g. confusing `gh pr create` with `gh pr merge`, or a plain
// `git push` with a force-push). Matching here short-circuits the LLM so these
// never flap. Anything NOT matched falls through to the LLM exactly as before,
// so this only ever ADDS approvals — never new denies.
//
// Note the explicit gh-pr subcommand allowlist: `merge` is deliberately absent
// (it must keep reaching the LLM, which denies it), and `git merge-base` is
// distinct from the denied `git merge`.
var safeBashCommandRe = regexp.MustCompile(`(?:^|[^a-z])gh pr (?:create|close|reopen|view|list|diff|checkout|comment|edit|ready|review|status)\b` +
	`|(?:^|[^a-z-])git push\b` +
	`|(?:^|[^a-z-])git merge-base\b`)

// dangerBashRe is a broad guard: if a command contains ANY of these markers we
// refuse to fast-approve it and fall through to the LLM (preserving every
// existing deny). It does not need to be exhaustive for safety — a missed
// danger marker just means the command is evaluated by the LLM as it is today.
// It only needs to be broad enough that we never fast-approve a chain that
// smuggles a destructive op alongside a safe-looking one.
var dangerBashRe = regexp.MustCompile(`gh pr merge` +
	`|git merge(?:\s|$|;|&|\||')` + // `git merge ...` but not `git merge-base`
	`|git reset --hard` +
	`|git clean -[a-z]*f` +
	`|--force(?:\s|$|;|&|\|)` + // bare --force (force-with-lease stripped before match)
	`|push -f\b|push --force` +
	`|rm -rf|rm -fr|rm -r -f` +
	`|drop table|drop database|truncate|delete from` +
	`|terraform apply|terraform destroy|--auto-approve` +
	`|kubectl delete|fly destroy|railway delete|vercel rm` +
	`|npm publish|pnpm publish|yarn publish|twine upload`)

// bashCommandDecision returns "approve" for a known-safe-but-misjudged command,
// or "" to fall through to the LLM. Conservative by construction: it only
// approves when the command hits a safe target AND contains no danger marker.
func bashCommandDecision(command string) string {
	lower := strings.ToLower(command)
	if !safeBashCommandRe.MatchString(lower) {
		return "" // not one of our targets — leave it to the LLM
	}
	// Strip force-with-lease so the bare `--force` danger check below doesn't
	// trip on it — `git push --force-with-lease` is routine post-rebase work.
	guard := strings.ReplaceAll(lower, "--force-with-lease", "")
	if dangerBashRe.MatchString(guard) {
		return "" // chain contains something destructive — let the LLM decide
	}
	return "approve"
}

// CheckPilotRules evaluates against pilot's own rule set.
// parsed is the pre-parsed toolInput JSON (nil if not JSON).
// Returns "approve", "deny", or "" (no match, fall through to LLM).
func CheckPilotRules(cfg *config.PilotConfig, toolName string, parsed map[string]any, cwd string) string {
	if bashTools[toolName] {
		if cmd := extractBashCommand(parsed); cmd != "" {
			return bashCommandDecision(cmd)
		}
		return "" // can't read the command — fall through
	}

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

// extractBashCommand pulls the shell command string from pre-parsed tool input.
// Handles Claude's Bash ("command") and Codex shell tools ("command" string or
// argv array under "command"/"argv").
func extractBashCommand(parsed map[string]any) string {
	if parsed == nil {
		return ""
	}
	for _, key := range []string{"command", "argv"} {
		switch v := parsed[key].(type) {
		case string:
			if v != "" {
				return v
			}
		case []any:
			parts := make([]string, 0, len(v))
			for _, p := range v {
				if s, ok := p.(string); ok {
					parts = append(parts, s)
				}
			}
			if len(parts) > 0 {
				return strings.Join(parts, " ")
			}
		}
	}
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
