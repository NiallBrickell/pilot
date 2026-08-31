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

// bashTools are the shell-exec tools whose command we inspect against the
// deterministic danger boundary.
//
// Monitor is here because it carries a shell command in the same "command"
// field Bash uses — it backgrounds a watch script (`tail -f … | grep`, a poll
// loop). It needs the same command inspection in normal and degraded mode.
//
// apply_patch is deliberately NOT here despite also having a "command" field:
// Codex puts patch *text* in it, and a diff that happens to contain "rm -rf"
// in a string literal would trip command danger markers on content it never
// executes.
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
// The Task* entries overlap builtinAutoApproved in claude_settings.go, which
// settles them one layer earlier. The two lists answer different questions —
// that one models what Claude Code approves on its own, this one is pilot's own
// judgment and applies to Codex too — so keep both; the overlap is harmless.
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

// dangerCommandRes are the complete boundary between deterministic approval and
// Haiku. Pilot's policy is a closed deny list, so sending every ordinary shell
// command to Haiku merely pays a model to say "approve". Inspectable commands
// now approve by default; only a concrete deny-list marker or an operation whose
// safety depends on context reaches the evaluator.
//
// Some markers intentionally retain context for Haiku to judge (for example,
// whether a Docker force-remove targets production). Routine cases explicitly
// approved by the prompt, including temp cleanup and non-destructive applies,
// are settled before that boundary.
var dangerCommandRes = []*regexp.Regexp{
	regexp.MustCompile(`\bgh\b[^\n;&|]*\bpr\s+merge\b`),
	regexp.MustCompile(`\bgit\b[^\n;&|]*\bmerge(?:\s|$|[;&|'])`), // not merge-base
	regexp.MustCompile(`\bgit\b[^\n;&|]*\breset\b[^\n;&|]*--hard\b`),
	regexp.MustCompile(`\bgit\b[^\n;&|]*\bpush\b[^\n;&|]*(?:--force(?:\s|$)|\s-f(?:\s|$))`),
	regexp.MustCompile(`\bfind\b[^\n;&|]*(?:-delete\b|-exec(?:dir)?\s+rm\b[^\n;&|]*(?:-[a-z]*r[a-z]*\b|--recursive\b))`),
	regexp.MustCompile(`\b(?:drop\s+(?:table|database|schema)|truncate\b|delete\s+from\b|update\s+\S+\s+set\b)`),
	regexp.MustCompile(`\bpsql\b[^\n;&|]*(?:\s-f\s|--file\b|\\i\b|\\o\b|\\copy\b)`),
	regexp.MustCompile(`\b(?:redis-cli\s+)?(?:flushall|flushdb)\b`),
	// Hosted-resource deletion. Global flags routinely sit between the binary
	// and the verb (`terraform -chdir=infra destroy`, `kubectl -n prod delete`,
	// `fly -a app destroy`), so the verb is matched anywhere in the same
	// command rather than adjacent to the binary. Over-matching only costs an
	// evaluator call; under-matching approved a destroy locally.
	regexp.MustCompile(`\b(?:railway|fly|flyctl|kubectl|vercel|terraform|encore)\b[^\n;&|]*\b(?:delete|destroy|rm)\b`),
	regexp.MustCompile(`\bterraform\b[^\n;&|]*\bapply\b[^\n;&|]*-auto-approve\b`),
	regexp.MustCompile(`\bdocker\b[^\n;&|]*\brm\b[^\n;&|]*(?:-[a-z]*f[a-z]*\b|--force\b)`),
	regexp.MustCompile(`(?:-x\s*|--request\s+)delete\b|["'](?:method|http_method)["']\s*:\s*["']delete["']`),
	regexp.MustCompile(`\b(?:npm|pnpm|yarn)\s+publish\b|\btwine\s+upload\b`),
}

// dangerousToolNameRe catches structured integrations where the operation is
// expressed by the tool name rather than a shell command. The verbs mirror
// destructive operation in the approval prompt's closed deny list. Ordinary
// create, apply, update and upload operations are intentionally absent because
// the prompt explicitly approves them. Word boundaries are underscore/hyphen
// aware, so `submit_result`, `update_assignment`, and `upload_file` stay routine.
var dangerousToolNameRe = regexp.MustCompile(`(?:^|[_-])(?:clean|delete|destroy|drop|erase|flush|force|merge|publish|purge|remove|reset|rm|truncate|wipe)(?:[_-]|$)`)

// destructiveBulkToolNameRe is intentionally compound: "bulk" alone is not
// dangerous, while updating every row or bulk deletion is the destructive
// database shape named by the prompt.
var destructiveBulkToolNameRe = regexp.MustCompile(`(?:^|[_-])(?:bulk[_-](?:delete|remove|truncate)|bulk[_-](?:update|mutate)[_-]all|update[_-]all[_-]rows?)(?:[_-]|$)`)

// dangerousOperationValueRe catches generic proxy tools whose operation is in
// structured input rather than the tool name, for example
// {"operation":"delete_dataset"}. Prose-bearing tools are handled by the
// always-safe list before their payload is scanned.
var dangerousOperationValueRe = regexp.MustCompile(`"(?:action|operation|operation_name|op|verb|command_name)"\s*:\s*"(?:[^"]*[_ -])?(?:clean|delete|destroy|drop|erase|flush|force|merge|publish|purge|remove|reset|rm|truncate|wipe)(?:[_ -][^"]*)?"`)

var destructiveBulkOperationValueRe = regexp.MustCompile(`"(?:action|operation|operation_name|op|verb|command_name)"\s*:\s*"(?:bulk[_ -](?:delete|remove|truncate)|bulk[_ -](?:update|mutate)[_ -]all|update[_ -]all[_ -]rows?)(?:[_ -][^"]*)?"`)

var (
	camelAcronymBoundaryRe = regexp.MustCompile(`([A-Z]+)([A-Z][a-z])`)
	camelWordBoundaryRe    = regexp.MustCompile(`([a-z0-9])([A-Z])`)
	rmInvocationRe         = regexp.MustCompile(`\brm\b[^\n;&|]*`)
	gitCleanInvocationRe   = regexp.MustCompile(`\bgit\b[^\n;&|]*\bclean\b[^\n;&|]*`)
	shellTokenRe           = regexp.MustCompile(`'[^']*'|"[^"]*"|\S+`)
)

func normalizeOperationName(name string) string {
	name = camelAcronymBoundaryRe.ReplaceAllString(name, `${1}_${2}`)
	name = camelWordBoundaryRe.ReplaceAllString(name, `${1}_${2}`)
	return strings.ToLower(name)
}

// alwaysSafeTools carry prose or file contents rather than an executable
// operation. Scanning their payload for words such as "DELETE" would mistake a
// patch, task prompt, or message *about* danger for the dangerous action itself.
var alwaysSafeTools = map[string]bool{
	"Edit": true, "Write": true, "MultiEdit": true, "NotebookEdit": true,
	"apply_patch": true, "Agent": true, "Task": true, "SendMessage": true,
	"SendUserFile": true, "PushNotification": true, "RemoteTrigger": true,
	"Workflow": true, "Artifact": true, "EnterWorktree": true,
	"ExitWorktree": true, "CronCreate": true, "CronUpdate": true,
	"update_plan": true,
}

// commandNeedsEvaluator reports whether an inspectable command carries a
// danger or ambiguity marker. force-with-lease is explicitly safe; stripping
// it prevents the conservative force-push marker from catching the substring.
func commandNeedsEvaluator(command string) bool {
	guard := strings.ReplaceAll(strings.ToLower(command), "--force-with-lease", "")
	if exfilSinkRe.MatchString(guard) {
		return true
	}
	if recursiveRMNeedsEvaluator(guard) || destructiveGitClean(guard) {
		return true
	}
	for _, pattern := range dangerCommandRes {
		if pattern.MatchString(guard) {
			return true
		}
	}
	return false
}

// recursiveRMNeedsEvaluator preserves the prompt's exact boundary: explicit
// /tmp and /private/tmp targets are safe; absolute/home paths and a sweep of
// the working tree reach Haiku. Scoped relative cleanup is routine. Variable
// paths remain ambiguous, and each invocation in a compound command is checked
// independently.
func recursiveRMNeedsEvaluator(command string) bool {
	for _, invocation := range rmInvocationRe.FindAllString(command, -1) {
		tokens := shellTokenRe.FindAllString(invocation, -1)
		if len(tokens) < 2 {
			continue
		}
		recursive := false
		var targets []string
		optionsDone := false
		for _, rawToken := range tokens[1:] {
			token := strings.Trim(rawToken, `'"`)
			if token == "--" {
				optionsDone = true
				continue
			}
			if !optionsDone && strings.HasPrefix(token, "-") {
				if token == "--recursive" || (!strings.HasPrefix(token, "--") && strings.Contains(token[1:], "r")) {
					recursive = true
				}
				continue
			}
			if strings.Contains(token, ">") {
				continue
			}
			targets = append(targets, token)
		}
		if !recursive {
			continue
		}
		if len(targets) == 0 {
			return true
		}
		for _, target := range targets {
			cleaned := filepath.Clean(target)
			if strings.ContainsAny(cleaned, "$`") {
				return true
			}
			if cleaned == "/tmp" || strings.HasPrefix(cleaned, "/tmp/") ||
				cleaned == "/private/tmp" || strings.HasPrefix(cleaned, "/private/tmp/") {
				continue
			}
			if strings.HasPrefix(cleaned, "/") || strings.HasPrefix(cleaned, "~") ||
				cleaned == "." || cleaned == ".." || strings.ContainsAny(cleaned, "*?[") {
				return true
			}
		}
	}
	return false
}

// destructiveGitClean matches the denied directory sweep, including split and
// long flag spellings. A force-clean of named files without -d is recoverable
// and is not on the prompt's closed deny list.
func destructiveGitClean(command string) bool {
	for _, invocation := range gitCleanInvocationRe.FindAllString(command, -1) {
		tokens := shellTokenRe.FindAllString(invocation, -1)
		cleanAt := -1
		for i := 1; i < len(tokens); i++ {
			token := strings.Trim(tokens[i], `'"`)
			if token == "-c" || token == "-C" || token == "--git-dir" || token == "--work-tree" || token == "--namespace" {
				i++ // each of these global flags consumes its next token
				continue
			}
			if strings.HasPrefix(token, "-") {
				continue
			}
			if token == "clean" {
				cleanAt = i
			}
			break // first non-global-option token is the git subcommand
		}
		if cleanAt < 0 {
			continue
		}
		force, directories, dryRun := false, false, false
		for _, rawToken := range tokens[cleanAt+1:] {
			token := strings.Trim(rawToken, `'"`)
			switch token {
			case "--force":
				force = true
			case "--directories":
				directories = true
			case "--dry-run":
				dryRun = true
			default:
				if strings.HasPrefix(token, "-") && !strings.HasPrefix(token, "--") {
					flags := token[1:]
					force = force || strings.Contains(flags, "f")
					directories = directories || strings.Contains(flags, "d")
					dryRun = dryRun || strings.Contains(flags, "n")
				}
			}
		}
		if force && directories && !dryRun {
			return true
		}
	}
	return false
}

// bashCommandDecision deterministically approves every inspectable routine
// command. An empty command is ambiguous; danger markers fall through to the
// evaluator for the final approve/deny judgment.
func bashCommandDecision(command string) string {
	if strings.TrimSpace(command) == "" || commandNeedsEvaluator(command) {
		return ""
	}
	return "approve"
}

// FailOpenDecision decides a tool call when the LLM evaluator is unavailable
// (API timeout/outage, even after retry). Pilot applies the same deterministic
// boundary as normal mode: inspectable routine calls approve, while danger
// markers and malformed or opaque input ask the user.
func FailOpenDecision(toolName, toolInput string) string {
	var parsed map[string]any
	rawInput := strings.TrimSpace(toolInput)
	if len(rawInput) == 0 || rawInput[0] != '{' ||
		json.Unmarshal([]byte(rawInput), &parsed) != nil || parsed == nil {
		return "ask"
	}

	if !bashTools[toolName] {
		if CheckPilotRules(&config.PilotConfig{}, toolName, parsed, "") == "approve" {
			return "approve"
		}
		return "ask"
	}
	cmd := extractBashCommand(parsed)
	if cmd == "" {
		return "ask" // can't inspect the command — stay conservative
	}
	if commandNeedsEvaluator(cmd) {
		return "ask"
	}
	return "approve"
}

// CheckPilotRules evaluates against pilot's own rule set.
// parsed is the pre-parsed toolInput JSON (nil if not JSON).
// Returns "approve", "deny", or "" (no match, fall through to LLM).
func CheckPilotRules(cfg *config.PilotConfig, toolName string, parsed map[string]any, cwd string) string {
	if parsed == nil {
		return ""
	}

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
	if alwaysSafeTools[toolName] {
		return "approve"
	}

	if !readOnlyTools[toolName] {
		// Unknown structured tools are routine by default once their input is
		// inspectable. Tool-name danger verbs, HTTP DELETE, SQL destruction and
		// capture endpoints are the residual cases that still need Haiku.
		encoded, err := json.Marshal(parsed)
		if err != nil {
			return ""
		}
		normalizedToolName := normalizeOperationName(toolName)
		lower := strings.ToLower(toolName + " " + string(encoded))
		normalizedPayload := strings.ToLower(normalizeOperationName(string(encoded)))
		if dangerousToolNameRe.MatchString(normalizedToolName) ||
			destructiveBulkToolNameRe.MatchString(normalizedToolName) ||
			dangerousOperationValueRe.MatchString(normalizedPayload) ||
			destructiveBulkOperationValueRe.MatchString(normalizedPayload) ||
			exfilSinkRe.MatchString(lower) ||
			commandNeedsEvaluator(lower) {
			return ""
		}
		return "approve"
	}

	// Reads never mutate or publish data. The approval prompt explicitly allows
	// inspecting the user's dotfiles, installed software, and other local paths,
	// so path location is not a reason to spend an evaluator call.
	return "approve"
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

// isWithinDir checks if target path is within or equal to root.
func isWithinDir(target, root string) bool {
	rel, err := filepath.Rel(root, target)
	if err != nil {
		return false
	}
	return !strings.HasPrefix(rel, "..")
}
