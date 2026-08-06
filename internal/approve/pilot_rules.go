// Layer 2: Pilot's own fast rules.
// Pattern-based rules that don't need an LLM call.
package approve

import (
	"encoding/json"
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
//
// Monitor is here because it carries a shell command in the same "command"
// field Bash uses — it backgrounds a watch script (`tail -f … | grep`, a poll
// loop). Leaving it out meant its command was never inspected by anything:
// FailOpenDecision approves every non-bash tool outright, so during an API
// outage a Monitor wrapping `rm -rf` would have been rubber-stamped without
// the danger-marker check Bash gets.
//
// apply_patch is deliberately NOT here despite also having a "command" field:
// Codex puts patch *text* in it, and a diff that happens to contain "rm -rf"
// in a string literal would trip dangerBashRe on content it never executes.
var bashTools = map[string]bool{
	"Bash": true, "shell": true, "local_shell": true, "Monitor": true,
}

// inertTools change nothing outside the session. They edit the session's own
// task list, pull a tool schema or a skill's instructions into context, or ask
// the user something. There is no command, path or destination for a rule to
// inspect and no judgment for the evaluator to make, so they are approved
// deterministically — otherwise the catch-all hook matcher would spend an LLM
// call per todo-list edit (TaskUpdate alone ran 669 times in one week).
//
// Anything that reaches outside the session once is deliberately absent and
// keeps falling through to the evaluator: Artifact (publishes a page to the
// web), SendUserFile, PushNotification, RemoteTrigger, SendMessage, Workflow
// (spawns agents), the Cron tools (schedule unattended runs), and
// EnterWorktree/ExitWorktree (mutate the repo).
var inertTools = map[string]bool{
	"TodoWrite": true, "ToolSearch": true, "Skill": true,
	"TaskCreate": true, "TaskUpdate": true, "TaskList": true,
	"TaskGet": true, "TaskOutput": true, "TaskStop": true,
	"AskUserQuestion": true, "ReportFindings": true,
	"EnterPlanMode": true, "ExitPlanMode": true, "ScheduleWakeup": true,
	"BashOutput": true, "KillShell": true, "NotebookRead": true,
}

// webTools read from the network without sending anything of the user's: a
// fetch is a GET of a URL the agent already knows, a search is a query string.
// The evaluator judged these on the *content* at the far end — refusing to read
// a vendor's API docs or terms page as "scraping", "PII harvesting" or
// "reconnaissance" — which is not a decision about the user's data or machine
// at all. Deciding them here keeps that judgment out of the loop entirely.
var webTools = map[string]bool{
	"WebFetch": true, "WebSearch": true, "web_search": true, "web_fetch": true,
}

// exfilSinkRe matches hosts that exist to capture whatever is sent to them.
// Fetching one is the single shape of web read that can carry data OUT, so it
// still goes to the LLM. Every other URL is a plain read.
var exfilSinkRe = regexp.MustCompile(`webhook\.site|requestbin|pipedream\.net|beeceptor|` +
	`ngrok\.io|ngrok-free\.app|trycloudflare\.com|` +
	`burpcollaborator|interact\.sh|oastify|canarytokens|` +
	`pastebin\.com|paste\.ee|hastebin|ghostbin|termbin|` +
	`transfer\.sh|0x0\.st|file\.io|anonfiles`)

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
//
// `git rebase` and `git reset` are here because the evaluator kept denying them
// by analogy to the deny list despite that list calling itself exhaustive. Both
// are safe to fast-approve only because dangerBashRe below still catches the one
// irreversible form, `git reset --hard`, and sends it to the LLM.
var safeBashCommandRe = regexp.MustCompile(`(?:^|[^a-z])gh pr (?:create|close|reopen|view|list|diff|checkout|comment|edit|ready|review|status)\b` +
	`|(?:^|[^a-z-])git push\b` +
	`|(?:^|[^a-z-])git merge-base\b` +
	`|(?:^|[^a-z-])git rebase\b` +
	`|(?:^|[^a-z-])git reset\b`)

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

// psqlRe matches an invocation of psql anywhere in a command chain.
var psqlRe = regexp.MustCompile(`(?:^|[^a-z])psql\b`)

// sqlMutationRe matches anything that stops a psql invocation counting as a
// pure read: SQL keywords that change data or schema, and the forms (-f/--file,
// \i, \o, \copy) that run or write SQL we can't see inline. `do` and `call`
// are SQL here but also shell-loop words — a psql chain wrapped in `for ...;
// do` just falls through to the LLM, which is the status quo, not a new deny.
var sqlMutationRe = regexp.MustCompile(`\b(insert|update|delete|drop|truncate|alter|create|grant|revoke|copy|call|do|merge|cluster|reindex|vacuum|refresh)\b` +
	`|(^|\s)-f\b|--file\b|\\i\b|\\o\b`)

// bashCommandDecision returns "approve" for a known-safe-but-misjudged command,
// or "" to fall through to the LLM. Conservative by construction: it only
// approves when the command hits a safe target AND contains no danger marker.
func bashCommandDecision(command string) string {
	lower := strings.ToLower(command)
	// A psql call whose inline SQL contains no mutating keyword is a read.
	// Decided here because the evaluator intermittently flags the inline
	// connection-string password as "credential exposure" — a hygiene opinion,
	// not a danger — and deterministic approval is the only way to stop the
	// flapping. Anything resembling a write, or SQL read from a file, still
	// goes to the LLM.
	readOnlyPsql := psqlRe.MatchString(lower) && !sqlMutationRe.MatchString(lower)
	if !safeBashCommandRe.MatchString(lower) && !readOnlyPsql {
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

// FailOpenDecision decides a tool call when the LLM evaluator is unavailable
// (API timeout/outage, even after retry). Pilot degrades to deny-list mode
// rather than blocking every request on transient infra trouble: approve
// unless the command carries a known danger marker. Bash commands matching
// dangerBashRe — and bash input we can't extract a command from — still ask.
func FailOpenDecision(toolName, toolInput string) string {
	if !bashTools[toolName] {
		return "approve"
	}
	var parsed map[string]any
	if len(toolInput) > 0 && toolInput[0] == '{' {
		_ = json.Unmarshal([]byte(toolInput), &parsed)
	}
	cmd := extractBashCommand(parsed)
	if cmd == "" {
		return "ask" // can't inspect the command — stay conservative
	}
	guard := strings.ReplaceAll(strings.ToLower(cmd), "--force-with-lease", "")
	if dangerBashRe.MatchString(guard) {
		return "ask"
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

	if webTools[toolName] {
		if u := extractURL(parsed); u != "" && exfilSinkRe.MatchString(strings.ToLower(u)) {
			return "" // a capture endpoint — the LLM should look at this one
		}
		return "approve"
	}

	if inertTools[toolName] {
		return "approve"
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

// extractURL pulls the target URL from pre-parsed web tool input. WebSearch
// carries a query rather than a URL, so it returns "" and the call is approved.
func extractURL(parsed map[string]any) string {
	if parsed == nil {
		return ""
	}
	for _, key := range []string{"url", "URL"} {
		if u, ok := parsed[key].(string); ok && u != "" {
			return u
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
