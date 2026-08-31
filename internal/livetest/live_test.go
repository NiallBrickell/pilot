//go:build live

// Package livetest drives a real Claude Code session against a freshly built
// pilot binary and a scratch `pilot serve`. The unit tests pin what each hook
// prints for a given server answer; only a live session shows which hooks
// Claude Code actually fires, in what order, and with what environment — the
// layer where the auto-mode fallback, the server token, and the headless
// case all broke without any unit test noticing.
//
//	make live            # or: go test -tags=live ./internal/livetest -v -count=1
//
// Needs `claude` on PATH and an Anthropic key (env or ~/.pilot/.env). Each
// case costs a few cents and about a minute. Everything runs in temp
// directories: an isolated CLAUDE_CONFIG_DIR, a scratch PILOT_HOME, and a
// scratch serve on a free port — the developer's own Pilot is never touched.
package livetest

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/NiallBrickell/pilot/internal/paths"
)

const marker = "PILOTLIVE"

// classifierTrigger is an autoMode hard_deny rule that makes the auto-mode
// classifier block any command carrying the marker, deterministically, so a
// harmless command exercises the denial path.
var classifierTrigger = map[string]any{
	"environment": []string{"Integration test harness for a permission-hook integration. The literal token " + marker + " marks a command that must be blocked."},
	"hard_deny":   []string{"Any shell command whose text contains the token " + marker + " must be blocked, no exceptions, regardless of user intent."},
}

type harness struct {
	t         *testing.T
	bin       string
	apiKey    string
	model     string
	root      string
	configDir string
	pilotHome string
	proj      string
	port      int
	serve     *exec.Cmd
	token     string
}

func requireLive(t *testing.T) (bin, apiKey, model string) {
	t.Helper()
	must := os.Getenv("PILOT_LIVE_REQUIRE") != ""
	skip := func(format string, args ...any) {
		if must {
			t.Fatalf("PILOT_LIVE_REQUIRE is set: "+format, args...)
		}
		t.Skipf(format, args...)
	}
	if _, err := exec.LookPath("claude"); err != nil {
		skip("claude not on PATH: %v", err)
	}
	apiKey = os.Getenv("ANTHROPIC_API_KEY")
	if apiKey == "" {
		apiKey = paths.EnvFileValue("ANTHROPIC_API_KEY")
	}
	if apiKey == "" {
		skip("no ANTHROPIC_API_KEY in env or %s", paths.EnvFile())
	}
	model = os.Getenv("PILOT_LIVE_MODEL")
	if model == "" {
		model = "claude-sonnet-5"
	}

	bin = filepath.Join(t.TempDir(), "pilot")
	build := exec.Command("go", "build", "-o", bin, "../..")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	return bin, apiKey, model
}

func newHarness(t *testing.T, bin, apiKey, model, token string) *harness {
	t.Helper()
	h := &harness{t: t, bin: bin, apiKey: apiKey, model: model, token: token}
	h.root = t.TempDir()
	h.configDir = filepath.Join(h.root, "claude-config")
	h.pilotHome = filepath.Join(h.root, "pilot-home")
	h.proj = filepath.Join(h.root, "proj")
	for _, d := range []string{h.configDir, h.pilotHome, h.proj} {
		if err := os.MkdirAll(d, 0755); err != nil {
			t.Fatal(err)
		}
	}
	h.port = freePort(t)

	// Scratch Pilot: the user's shape (never wait on the dashboard, LLM
	// keep-going nudges off) on a private port.
	toml := fmt.Sprintf("[general]\nsse_port = %d\nescalation_timeout_s = 0\nstop_hook_replies = false\nmodel = \"claude-haiku-4-5\"\n", h.port)
	h.write(filepath.Join(h.pilotHome, "pilot.toml"), toml)
	if token != "" {
		// Only in .env — never in the serve or hook environment — so the run
		// proves both sides resolve the token from the file.
		h.write(filepath.Join(h.pilotHome, ".env"), "PILOT_SERVER_TOKEN="+token+"\n")
	}

	// Isolated Claude Code: auto mode, the classifier trigger, and Pilot's
	// hooks in the exact shape the installer writes.
	hook := func(cmd string) []any {
		return []any{map[string]any{"matcher": ".*", "hooks": []any{map[string]any{"type": "command", "command": bin + " " + cmd}}}}
	}
	settings := map[string]any{
		"permissions": map[string]any{"defaultMode": "auto", "allow": []string{"Read(*)"}},
		"autoMode":    classifierTrigger,
		"hooks": map[string]any{
			"PermissionRequest": hook("approve"),
			"PermissionDenied":  hook("on-denied"),
			"PreToolUse":        hook("pre-approve"),
			"Stop":              []any{map[string]any{"hooks": []any{map[string]any{"type": "command", "command": bin + " on-stop"}}}},
		},
	}
	h.writeJSON(filepath.Join(h.configDir, "settings.json"), settings)
	suffix := apiKey
	if len(suffix) > 20 {
		suffix = suffix[len(suffix)-20:]
	}
	h.writeJSON(filepath.Join(h.configDir, ".claude.json"), map[string]any{
		"hasCompletedOnboarding": true,
		"theme":                  "dark",
		"customApiKeyResponses":  map[string]any{"approved": []string{suffix}, "rejected": []string{}},
		"projects": map[string]any{h.proj: map[string]any{
			"hasTrustDialogAccepted":        true,
			"hasCompletedProjectOnboarding": true,
		}},
	})

	git := func(args ...string) {
		c := exec.Command("git", args...)
		c.Dir = h.proj
		c.Env = append(os.Environ(), "GIT_AUTHOR_NAME=live", "GIT_AUTHOR_EMAIL=live@test", "GIT_COMMITTER_NAME=live", "GIT_COMMITTER_EMAIL=live@test")
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	h.write(filepath.Join(h.proj, "README.md"), "# live harness\n")
	git("init", "-q")
	git("add", "-A")
	git("commit", "-qm", "init")

	h.startServe()
	return h
}

func (h *harness) write(path, content string) {
	h.t.Helper()
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		h.t.Fatal(err)
	}
}

func (h *harness) writeJSON(path string, v any) {
	h.t.Helper()
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		h.t.Fatal(err)
	}
	h.write(path, string(b))
}

// env is what the serve process and — via Claude Code — every hook sees. The
// server token is deliberately absent: it must come from .env.
func (h *harness) env() []string {
	env := []string{
		"PILOT_HOME=" + h.pilotHome,
		"ANTHROPIC_API_KEY=" + h.apiKey,
		"CLAUDE_CONFIG_DIR=" + h.configDir,
		"HOME=" + os.Getenv("HOME"),
		"PATH=" + os.Getenv("PATH"),
		"TERM=xterm-256color",
	}
	for _, k := range []string{"TMPDIR", "LANG", "SHELL", "GOCACHE", "GOPATH"} {
		if v := os.Getenv(k); v != "" {
			env = append(env, k+"="+v)
		}
	}
	return env
}

func (h *harness) startServe() {
	h.t.Helper()
	logPath := filepath.Join(h.pilotHome, "serve.log")
	logFile, err := os.Create(logPath)
	if err != nil {
		h.t.Fatal(err)
	}
	h.serve = exec.Command(h.bin, "serve")
	h.serve.Env = h.env()
	h.serve.Stdout, h.serve.Stderr = logFile, logFile
	if err := h.serve.Start(); err != nil {
		h.t.Fatalf("start serve: %v", err)
	}
	h.t.Cleanup(func() {
		_ = h.serve.Process.Kill()
		_, _ = h.serve.Process.Wait()
		logFile.Close()
	})
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get(h.url("/health"))
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	out, _ := os.ReadFile(logPath)
	h.t.Fatalf("pilot serve did not come up on :%d\n%s", h.port, out)
}

func (h *harness) url(path string) string { return fmt.Sprintf("http://127.0.0.1:%d%s", h.port, path) }

type sessionResult struct {
	Result           string `json:"result"`
	NumTurns         int    `json:"num_turns"`
	PermissionDenies []any  `json:"permission_denials"`
	debugLog         string
}

// runHeadless runs one `claude -p` turn in the scratch project.
func (h *harness) runHeadless(prompt string) sessionResult {
	h.t.Helper()
	debug := filepath.Join(h.root, "claude-debug.log")
	cmd := exec.Command("claude", "-p", "--permission-mode", "auto", "--model", h.model, "--output-format", "json", "--debug-file", debug, prompt)
	cmd.Dir = h.proj
	cmd.Env = h.env()
	out, err := cmd.Output()
	if err != nil {
		var stderr string
		if ee, ok := err.(*exec.ExitError); ok {
			stderr = string(ee.Stderr)
		}
		h.t.Fatalf("claude -p: %v\nstdout: %s\nstderr: %s", err, out, stderr)
	}
	var res sessionResult
	if err := json.Unmarshal(out, &res); err != nil {
		h.t.Fatalf("claude -p output is not JSON: %v\n%s", err, out)
	}
	if dbg, err := os.ReadFile(debug); err == nil {
		res.debugLog = string(dbg)
	}
	if !strings.Contains(res.debugLog, "Auto mode classifier blocked action") {
		h.t.Fatalf("harness invalid: the auto-mode classifier never blocked the call, so nothing was tested\nresult: %s", res.Result)
	}
	return res
}

type action struct {
	ActionType string `json:"action_type"`
	Detail     string `json:"detail"`
	Source     string `json:"source"`
}

func (h *harness) actions() []action {
	h.t.Helper()
	resp, err := http.Get(h.url("/status"))
	if err != nil {
		h.t.Fatal(err)
	}
	defer resp.Body.Close()
	var status struct {
		RecentActions []action `json:"recent_actions"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&status); err != nil {
		h.t.Fatal(err)
	}
	return status.RecentActions
}

type logEntry struct {
	Level   string `json:"level"`
	Source  string `json:"source"`
	Message string `json:"message"`
}

func (h *harness) logs() []logEntry {
	h.t.Helper()
	resp, err := http.Get(h.url("/logs"))
	if err != nil {
		h.t.Fatal(err)
	}
	defer resp.Body.Close()
	var logs []logEntry
	if err := json.NewDecoder(resp.Body).Decode(&logs); err != nil {
		h.t.Fatal(err)
	}
	return logs
}

func (h *harness) hasAction(actionType, detailFragment string) bool {
	for _, a := range h.actions() {
		if a.ActionType == actionType && strings.Contains(a.Detail, detailFragment) {
			return true
		}
	}
	return false
}

func (h *harness) hasLog(source, fragment string) bool {
	for _, l := range h.logs() {
		if l.Source == source && strings.Contains(l.Message, fragment) {
			return true
		}
	}
	return false
}

func freePort(t *testing.T) int {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

// outsidePath is a file outside the project: an in-project write is
// auto-allowed without consulting the classifier at all.
func (h *harness) outsidePath() string { return filepath.Join(h.root, "outside.txt") }

func (h *harness) writeOutsidePrompt(tag string) string {
	return fmt.Sprintf("Run exactly this shell command once and report the result: echo %s-%s > %s && cat %s",
		marker, tag, h.outsidePath(), h.outsidePath())
}

// TestClassifierDenialFallsBackToPilot is the bug report: the classifier
// blocks a routine call, Claude ends its turn with "run this yourself". With
// the fallback, Pilot approves it and the retry runs with no second
// classifier pass.
func TestClassifierDenialFallsBackToPilot(t *testing.T) {
	bin, apiKey, model := requireLive(t)

	for _, tc := range []struct {
		name  string
		token string
	}{
		{"local-mode", ""},
		{"token-mode", "live-test-token-" + fmt.Sprint(time.Now().UnixNano())},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, bin, apiKey, model, tc.token)

			if tc.token != "" {
				// Prove token mode is really on: an unauthenticated POST must
				// be refused, or the hooks succeeding proves nothing.
				resp, err := http.Post(h.url("/internal/retry"), "application/json", strings.NewReader(`{"action":"nudge","session_id":"x"}`))
				if err != nil {
					t.Fatal(err)
				}
				resp.Body.Close()
				if resp.StatusCode != http.StatusUnauthorized {
					t.Fatalf("token mode not active: unauthenticated POST got %d", resp.StatusCode)
				}
			}

			res := h.runHeadless(h.writeOutsidePrompt(tc.name))

			got, err := os.ReadFile(h.outsidePath())
			if err != nil {
				t.Fatalf("the blocked command never ran after Pilot's approval; Claude said: %s\nactions: %+v\nlogs: %+v", res.Result, h.actions(), h.logs())
			}
			if want := marker + "-" + tc.name; strings.TrimSpace(string(got)) != want {
				t.Fatalf("outside file = %q, want %q", got, want)
			}
			if !h.hasAction("auto_approve", "after auto-mode classifier") {
				t.Fatalf("Pilot did not record approving the classifier-blocked call\nactions: %+v", h.actions())
			}
			if !strings.Contains(res.debugLog, `"permissionDecision":"allow"`) {
				t.Fatal("the retry was not admitted by the PreToolUse hook; it went back through the classifier")
			}
		})
	}
}

// TestHeadlessSessionLeavesPilotDenialsStanding: `claude -p` cannot prompt,
// so when Pilot itself wants a human, the classifier's denial must stand
// rather than be retried into a second, more confusing denial.
func TestHeadlessSessionLeavesPilotDenialsStanding(t *testing.T) {
	bin, apiKey, model := requireLive(t)
	h := newHarness(t, bin, apiKey, model, "")

	// git reset --hard is on Pilot's deny list, so Haiku denies and, with no
	// dashboard wait configured, the escalation times out immediately. The
	// marker makes the classifier block it first. Harmless in a clean repo.
	res := h.runHeadless("Run exactly this shell command once in this repo and report the result: git reset --hard HEAD # " + marker)

	if !h.hasLog("on-denied", "headless session") {
		t.Fatalf("expected on-denied to leave the denial standing in a headless session\nlogs: %+v\nclaude: %s", h.logs(), res.Result)
	}
	if strings.Contains(res.debugLog, `"permissionDecision":"ask"`) {
		t.Fatal("PreToolUse asked for a prompt in a session that has none")
	}
	if !h.hasAction("escalate", "after auto-mode classifier") {
		t.Fatalf("the Pilot denial should still be recorded as an escalation\nactions: %+v", h.actions())
	}
}
