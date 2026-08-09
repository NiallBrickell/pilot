package approve

import (
	"encoding/json"
	"testing"

	"github.com/NiallBrickell/pilot/internal/config"
)

// TestBashCommandDecision pins the deterministic Layer-2 allowlist: safe-but-
// frequently-misjudged commands must fast-approve, while every genuinely
// dangerous command must fall through ("") so the LLM can still deny it.
func TestBashCommandDecision(t *testing.T) {
	cases := []struct {
		name string
		cmd  string
		want string
	}{
		// --- Safe-but-misjudged: must fast-approve ---
		{"gh pr create", `cd /tmp/x && gh pr create --title "feat: x" --body "y"`, "approve"},
		{"gh pr create + close chain", `gh pr create --label e2e --title "t" --body "b" && gh pr close 1061 --comment "superseded"`, "approve"},
		{"gh pr view readonly", `GH_TOKEN=$TOKEN gh pr view 1022 --json state`, "approve"},
		{"gh pr comment", `gh pr comment 5 --body "looks good"`, "approve"},
		{"git push plain", `cd /tmp/x && go test ./... && git push -u origin feat/widget-modalities-clean`, "approve"},
		{"git push to main", `git push origin main`, "approve"},
		{"git push force-with-lease", `git push --force-with-lease origin feat/x`, "approve"},
		{"git push token url", `git push https://x-access-token:ghp_abc@github.com/o/r.git HEAD`, "approve"},
		{"git merge-base is-ancestor", `git log --oneline -1 && git merge-base --is-ancestor 891abc HEAD`, "approve"},
		{"git rebase onto main", `cd /tmp/x && git rebase origin/main`, "approve"},
		{"git rebase continue", `git add -A && git rebase --continue`, "approve"},
		{"git rebase abort", `git rebase --abort`, "approve"},
		{"git reset soft", `git reset --soft origin/main && git status --short`, "approve"},
		{"git reset mixed path", `git reset HEAD backend/x.go`, "approve"},

		// --- Read-only psql: must fast-approve (inline passwords in the
		// connection string made the evaluator flap on "credential exposure") ---
		{"psql select inline password", `cd /tmp && psql "postgresql://claude_code:***REDACTED***@203.0.113.71:5432/integration?sslmode=require" -x -c "SELECT i.id, i.status, i.created_at, i.updated_at, i.refresh_token_expires_at, length(i.credentials) AS cred_len FROM integration i JOIN integration_config c ON c.id = i.config_id WHERE i.id IN ('7acccd92');" 2>&1 | head -60`, "approve"},
		{"psql dt meta", `PGPASSWORD='x' psql "postgresql://claude_code@203.0.113.71:5432/knowledge" -c '\dt'`, "approve"},
		{"psql select count chain", `psql "postgresql://u:p@203.0.113.71:5432/knowledge?sslmode=require" -c 'select count(*) from assets'`, "approve"},

		// --- psql that could write: must fall through to the LLM ("") ---
		{"psql drop database", `docker exec sqldb-acme-mx62 psql -U postgres -c 'DROP DATABASE dataset'`, ""},
		{"psql update", `psql "postgresql://u:p@host:5432/db" -c "UPDATE integration SET status = 'active'"`, ""},
		{"psql insert", `psql -c "insert into t values (1)"`, ""},
		{"psql delete where", `psql -c "delete from t where id = 1"`, ""},
		{"psql sql from file", `psql "postgresql://u:p@host:5432/db" -f migration.sql`, ""},
		{"psql include meta", `psql -c '\i /tmp/run.sql'`, ""},
		{"psql create table", `psql -c "create table t (id int)"`, ""},
		{"psql in for loop", `for db in a b; do psql -c "select 1" "$db"; done`, ""},

		// --- Genuinely dangerous: must fall through to the LLM ("") ---
		{"gh pr merge", `gh pr merge 1070 --merge`, ""},
		{"gh pr merge admin", `GH_TOKEN=$TOKEN gh pr merge 9 --repo o/r --merge --admin`, ""},
		{"gh pr create then merge", `gh pr create --title t --body b && gh pr merge 5 --squash`, ""},
		{"git merge ff-only", `git checkout main && git merge --ff-only feat/x`, ""},
		{"git push force", `git push --force origin main`, ""},
		{"git push -f", `git push -f origin main`, ""},
		{"git reset hard before push", `git reset --hard HEAD~1 && git push --force-with-lease`, ""},
		{"git reset hard alone", `git reset --hard origin/main`, ""},
		{"git rebase then reset hard", `git rebase origin/main || git reset --hard ORIG_HEAD`, ""},
		{"rm -rf alongside push", `git push origin x && rm -rf /tmp/junk`, ""},
		{"terraform apply", `terraform apply -auto-approve -target=x`, ""},

		// --- Not our concern: fall through unchanged ---
		{"git status", `git status --short`, ""},
		{"go build", `go build ./...`, ""},
		{"empty", ``, ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := bashCommandDecision(tc.cmd); got != tc.want {
				t.Errorf("bashCommandDecision(%q) = %q, want %q", tc.cmd, got, tc.want)
			}
		})
	}
}

// TestFailOpenDecision pins the degraded mode used when the LLM evaluator is
// unreachable: everything approves except bash commands with danger markers
// (and bash input we can't read a command out of), which still ask.
func TestFailOpenDecision(t *testing.T) {
	cases := []struct {
		name      string
		toolName  string
		toolInput string
		want      string
	}{
		{"routine bash", "Bash", `{"command":"cd /tmp/x && ls -la && cat package.json"}`, "approve"},
		{"go test", "Bash", `{"command":"go test ./..."}`, "approve"},
		{"force-with-lease ok", "Bash", `{"command":"git push --force-with-lease origin feat/x"}`, "approve"},
		{"rm -rf asks", "Bash", `{"command":"rm -rf /tmp/junk"}`, "ask"},
		{"force push asks", "Bash", `{"command":"git push --force origin main"}`, "ask"},
		{"gh pr merge asks", "Bash", `{"command":"gh pr merge 5 --squash"}`, "ask"},
		{"danger smuggled in chain", "Bash", `{"command":"ls && git reset --hard HEAD~3"}`, "ask"},
		{"unreadable bash input asks", "Bash", `{"weird":"shape"}`, "ask"},
		{"non-json bash input asks", "Bash", `not json`, "ask"},
		{"codex shell", "shell", `{"argv":["git","status"]}`, "approve"},
		{"read tool approves", "Read", `{"file_path":"/etc/hosts"}`, "approve"},
		{"edit tool approves", "Edit", `{"file_path":"/tmp/x/main.go"}`, "approve"},

		// Monitor carries a shell command. Before it was classed as a bash tool
		// it took the blanket non-bash approve below, so an outage was the one
		// moment its command went completely uninspected.
		{"monitor wait loop", "Monitor", `{"command":"until [ -s \"$f\" ]; do sleep 5; done","description":"wait"}`, "approve"},
		{"monitor tail grep", "Monitor", `{"command":"tail -f deploy.log | grep --line-buffered ERROR"}`, "approve"},
		{"monitor smuggling rm -rf asks", "Monitor", `{"command":"tail -f x.log; rm -rf /tmp/junk"}`, "ask"},
		{"monitor force push asks", "Monitor", `{"command":"while true; do git push --force origin main; sleep 60; done"}`, "ask"},
		{"monitor websocket has no command", "Monitor", `{"ws":{"url":"wss://events.example.com/stream"}}`, "ask"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := FailOpenDecision(tc.toolName, tc.toolInput); got != tc.want {
				t.Errorf("FailOpenDecision(%q, %q) = %q, want %q", tc.toolName, tc.toolInput, got, tc.want)
			}
		})
	}
}

// TestToolCoverage pins how Layer 2 treats the tools the catch-all hook matcher
// now delivers. Three groups, and the boundary between them is the point:
// session bookkeeping settles here so the widened matcher costs nothing per
// call; anything carrying a command or reaching outside the session must not.
func TestToolCoverage(t *testing.T) {
	cases := []struct {
		name  string
		tool  string
		input string
		want  string
	}{
		// Inert: no command, no destination, nothing for the evaluator to weigh.
		{"task update", "TaskUpdate", `{"task_id":"a1","status":"completed"}`, "approve"},
		{"task create", "TaskCreate", `{"prompt":"fix the flaky test"}`, "approve"},
		{"tool search", "ToolSearch", `{"query":"select:Monitor","max_results":2}`, "approve"},
		{"skill load", "Skill", `{"skill":"commit"}`, "approve"},
		{"ask user question", "AskUserQuestion", `{"questions":[]}`, "approve"},
		{"exit plan mode", "ExitPlanMode", `{"plan":"do the thing"}`, "approve"},

		// Command-carrying: must be inspected, never blanket-approved.
		{"monitor safe loop", "Monitor", `{"command":"tail -f app.log | grep --line-buffered ERROR"}`, ""},
		{"monitor with rm -rf", "Monitor", `{"command":"tail -f x.log; rm -rf /tmp/junk"}`, ""},
		{"monitor running git push", "Monitor", `{"command":"git push origin main"}`, "approve"},

		// Outward-facing: these leave the session, so they keep reaching the LLM.
		{"artifact publish", "Artifact", `{"file_path":"/tmp/report.html","favicon":"📊"}`, ""},
		{"send user file", "SendUserFile", `{"files":["/tmp/report.md"],"status":"normal"}`, ""},
		{"remote trigger", "RemoteTrigger", `{"prompt":"deploy staging"}`, ""},
		{"cron create", "CronCreate", `{"schedule":"0 9 * * *","prompt":"daily sweep"}`, ""},
		{"enter worktree", "EnterWorktree", `{"branch":"feat/x"}`, ""},
		{"workflow", "Workflow", `{"script":"export const meta = {}"}`, ""},
	}

	cfg := &config.PilotConfig{}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var parsed map[string]any
			if err := json.Unmarshal([]byte(tc.input), &parsed); err != nil {
				t.Fatalf("bad test input: %v", err)
			}
			if got := CheckPilotRules(cfg, tc.tool, parsed, "/Users/niall/work/projects/pilot"); got != tc.want {
				t.Errorf("CheckPilotRules(%s) = %q, want %q", tc.tool, got, tc.want)
			}
		})
	}
}

func TestExtractBashCommand(t *testing.T) {
	if got := extractBashCommand(map[string]any{"command": "git push"}); got != "git push" {
		t.Errorf("command string: got %q", got)
	}
	if got := extractBashCommand(map[string]any{"argv": []any{"git", "push"}}); got != "git push" {
		t.Errorf("argv array: got %q", got)
	}
	if got := extractBashCommand(map[string]any{}); got != "" {
		t.Errorf("missing: got %q", got)
	}
}
