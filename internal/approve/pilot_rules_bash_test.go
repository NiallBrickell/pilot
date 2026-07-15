package approve

import "testing"

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

		// --- Genuinely dangerous: must fall through to the LLM ("") ---
		{"gh pr merge", `gh pr merge 1070 --merge`, ""},
		{"gh pr merge admin", `GH_TOKEN=$TOKEN gh pr merge 9 --repo o/r --merge --admin`, ""},
		{"gh pr create then merge", `gh pr create --title t --body b && gh pr merge 5 --squash`, ""},
		{"git merge ff-only", `git checkout main && git merge --ff-only feat/x`, ""},
		{"git push force", `git push --force origin main`, ""},
		{"git push -f", `git push -f origin main`, ""},
		{"git reset hard before push", `git reset --hard HEAD~1 && git push --force-with-lease`, ""},
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
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := FailOpenDecision(tc.toolName, tc.toolInput); got != tc.want {
				t.Errorf("FailOpenDecision(%q, %q) = %q, want %q", tc.toolName, tc.toolInput, got, tc.want)
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
