package cmd

import (
	"bytes"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/NiallBrickell/pilot/internal/config"
)

// fakeServe stands in for pilot serve: it answers /internal/evaluate with a
// scripted decision, /internal/pending with a scripted outcome, and keeps the
// retry hand-off's register/claim traffic for assertions.
type fakeServe struct {
	mu        sync.Mutex
	evalCalls int
	evaluate  map[string]any
	pending   map[string]any
	claim     map[string]any
	nudge     map[string]any
	retryLog  []map[string]any
	srv       *httptest.Server
	cfg       *config.PilotConfig
}

func newFakeServe(t *testing.T) *fakeServe {
	t.Helper()
	f := &fakeServe{claim: map[string]any{"decision": ""}, nudge: map[string]any{"decision": ""}}
	mux := http.NewServeMux()
	mux.HandleFunc("/internal/evaluate", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.evalCalls++
		out := f.evaluate
		f.mu.Unlock()
		_ = json.NewEncoder(w).Encode(out)
	})
	mux.HandleFunc("/internal/pending", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		out := f.pending
		f.mu.Unlock()
		_ = json.NewEncoder(w).Encode(out)
	})
	mux.HandleFunc("/internal/retry", func(w http.ResponseWriter, r *http.Request) {
		var req map[string]any
		_ = json.NewDecoder(r.Body).Decode(&req)
		f.mu.Lock()
		f.retryLog = append(f.retryLog, req)
		var out map[string]any
		switch req["action"] {
		case "register":
			out = map[string]any{"registered": true, "key": "k"}
		case "claim":
			out = f.claim
		case "nudge":
			out = f.nudge
		}
		f.mu.Unlock()
		_ = json.NewEncoder(w).Encode(out)
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	_, portStr, _ := net.SplitHostPort(strings.TrimPrefix(f.srv.URL, "http://"))
	var port int
	for _, c := range portStr {
		port = port*10 + int(c-'0')
	}
	f.cfg = &config.PilotConfig{}
	f.cfg.General.SSEPort = port
	return f
}

func (f *fakeServe) registered() []map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []map[string]any
	for _, r := range f.retryLog {
		if r["action"] == "register" {
			out = append(out, r)
		}
	}
	return out
}

// captureStdout runs fn and returns what it printed; hook answers go to stdout.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	orig := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	done := make(chan string)
	go func() {
		var buf bytes.Buffer
		_, _ = io.Copy(&buf, r)
		done <- buf.String()
	}()
	fn()
	w.Close()
	os.Stdout = orig
	return strings.TrimSpace(<-done)
}

func deniedInput() hookInput {
	return hookInput{
		Event:     eventPermissionDenied,
		ToolName:  "Bash",
		ToolInput: `{"command":"terraform apply tfplan","description":"Apply prod plan"}`,
		Cwd:       "/work/infra",
		SessionID: "sess-1",
		Reason:    "Blocked by classifier",
	}
}

func TestPermissionDeniedApprovalRegistersAllowAndAsksForRetry(t *testing.T) {
	t.Setenv("PILOT_HOME", t.TempDir())
	f := newFakeServe(t)

	out := captureStdout(t, func() {
		res := &evalResult{Decision: "approve", Reason: "matched pilot rule", Source: "pilot_rules"}
		if err := handlePermissionDeniedResult(f.cfg, deniedInput(), res, time.Now()); err != nil {
			t.Fatal(err)
		}
	})
	if !strings.Contains(out, `"hookEventName":"PermissionDenied"`) || !strings.Contains(out, `"retry":true`) {
		t.Fatalf("expected a retry answer, got %q", out)
	}
	regs := f.registered()
	if len(regs) != 1 || regs[0]["decision"] != retryAllow || regs[0]["session_id"] != "sess-1" || regs[0]["tool_name"] != "Bash" {
		t.Fatalf("expected one allow registration for the session, got %v", regs)
	}
}

func TestPermissionDeniedHumanApprovalOnDashboardRegistersAllow(t *testing.T) {
	t.Setenv("PILOT_HOME", t.TempDir())
	f := newFakeServe(t)
	f.cfg.General.EscalationTimeoutS = 5
	f.pending = map[string]any{"outcome": "approved", "resolved_by": "human"}

	out := captureStdout(t, func() {
		res := &evalResult{Decision: "deny", Reason: "terraform apply on production", Source: "haiku"}
		if err := handlePermissionDeniedResult(f.cfg, deniedInput(), res, time.Now()); err != nil {
			t.Fatal(err)
		}
	})
	if !strings.Contains(out, `"retry":true`) {
		t.Fatalf("human approval must ask the model to retry, got %q", out)
	}
	regs := f.registered()
	if len(regs) != 1 || regs[0]["decision"] != retryAllow {
		t.Fatalf("expected an allow registration, got %v", regs)
	}
}

func TestPermissionDeniedHumanRejectionLeavesDenialStanding(t *testing.T) {
	t.Setenv("PILOT_HOME", t.TempDir())
	f := newFakeServe(t)
	f.cfg.General.EscalationTimeoutS = 5
	f.pending = map[string]any{"outcome": "rejected", "resolved_by": "human"}

	out := captureStdout(t, func() {
		res := &evalResult{Decision: "deny", Reason: "terraform apply on production", Source: "haiku"}
		if err := handlePermissionDeniedResult(f.cfg, deniedInput(), res, time.Now()); err != nil {
			t.Fatal(err)
		}
	})
	if out != "" {
		t.Fatalf("a human rejection must not ask for a retry, got %q", out)
	}
	if regs := f.registered(); len(regs) != 0 {
		t.Fatalf("nothing should be registered after a rejection, got %v", regs)
	}
}

func TestPermissionDeniedEscalationTimeoutRoutesRetryToUserPrompt(t *testing.T) {
	t.Setenv("PILOT_HOME", t.TempDir())
	f := newFakeServe(t)
	f.cfg.General.EscalationTimeoutS = 0 // the user's config: never wait on the dashboard

	out := captureStdout(t, func() {
		res := &evalResult{Decision: "deny", Reason: "gh pr merge is a human decision", Source: "haiku"}
		if err := handlePermissionDeniedResult(f.cfg, deniedInput(), res, time.Now()); err != nil {
			t.Fatal(err)
		}
	})
	if !strings.Contains(out, `"retry":true`) {
		t.Fatalf("a timed-out escalation must still bring the call back as a prompt, got %q", out)
	}
	regs := f.registered()
	if len(regs) != 1 || regs[0]["decision"] != retryAsk {
		t.Fatalf("expected an ask registration, got %v", regs)
	}
}

func TestPermissionDeniedInfrastructureAskRoutesToUserPrompt(t *testing.T) {
	t.Setenv("PILOT_HOME", t.TempDir())
	f := newFakeServe(t)

	out := captureStdout(t, func() {
		res := &evalResult{Decision: "ask", Reason: "monthly evaluator spend cap reached", Source: ""}
		if err := handlePermissionDeniedResult(f.cfg, deniedInput(), res, time.Now()); err != nil {
			t.Fatal(err)
		}
	})
	if !strings.Contains(out, `"retry":true`) {
		t.Fatalf("expected retry, got %q", out)
	}
	if regs := f.registered(); len(regs) != 1 || regs[0]["decision"] != retryAsk {
		t.Fatalf("expected an ask registration, got %v", regs)
	}
}

func TestPermissionDeniedSettingsDenyLeavesDenialStanding(t *testing.T) {
	t.Setenv("PILOT_HOME", t.TempDir())
	f := newFakeServe(t)
	f.cfg.General.EscalationTimeoutS = 5
	f.pending = map[string]any{"outcome": "approved", "resolved_by": "human"} // must never be consulted

	out := captureStdout(t, func() {
		res := &evalResult{Decision: "deny", Reason: "matched Claude Code settings", Source: "claude_settings"}
		if err := handlePermissionDeniedResult(f.cfg, deniedInput(), res, time.Now()); err != nil {
			t.Fatal(err)
		}
	})
	if out != "" || len(f.registered()) != 0 {
		t.Fatalf("a settings deny rule is the user's standing order; got output %q registrations %v", out, f.registered())
	}
}

func TestPermissionDeniedWithoutServeStaysSilent(t *testing.T) {
	t.Setenv("PILOT_HOME", t.TempDir())
	cfg := &config.PilotConfig{}
	cfg.General.SSEPort = 1 // nothing listens here

	out := captureStdout(t, func() {
		res := &evalResult{Decision: "approve", Reason: "matched pilot rule", Source: "pilot_rules"}
		if err := handlePermissionDeniedResult(cfg, deniedInput(), res, time.Now()); err != nil {
			t.Fatal(err)
		}
	})
	if out != "" {
		t.Fatalf("without a registered decision a retry would just be denied again; got %q", out)
	}
}

func TestPreToolUseClaimAnswersOnlyForDecidedRetries(t *testing.T) {
	f := newFakeServe(t)
	h := hookInput{Event: eventPreToolUse, ToolName: "Bash", ToolInput: `{"command":"terraform apply tfplan"}`, SessionID: "sess-1"}

	if out := captureStdout(t, func() { _ = runPreToolUseClaim(f.cfg, h) }); out != "" {
		t.Fatalf("an undecided call must leave Claude's flow untouched, got %q", out)
	}

	f.claim = map[string]any{"decision": "allow", "reason": "matched pilot rule"}
	out := captureStdout(t, func() { _ = runPreToolUseClaim(f.cfg, h) })
	if !strings.Contains(out, `"hookEventName":"PreToolUse"`) || !strings.Contains(out, `"permissionDecision":"allow"`) {
		t.Fatalf("expected a PreToolUse allow, got %q", out)
	}

	f.claim = map[string]any{"decision": "ask", "reason": "no human answer"}
	out = captureStdout(t, func() { _ = runPreToolUseClaim(f.cfg, h) })
	if !strings.Contains(out, `"permissionDecision":"ask"`) {
		t.Fatalf("expected a PreToolUse ask, got %q", out)
	}

	f.mu.Lock()
	var stages []any
	for _, r := range f.retryLog {
		if r["action"] == "claim" {
			stages = append(stages, r["stage"])
		}
	}
	f.mu.Unlock()
	for _, st := range stages {
		if st != retryStagePreToolUse {
			t.Fatalf("PreToolUse must claim at its own stage, got %v", stages)
		}
	}

	if out := captureStdout(t, func() { _ = runPreToolUseClaim(&config.PilotConfig{General: config.GeneralConfig{SSEPort: 1}}, h) }); out != "" {
		t.Fatalf("serve down must fail open silently, got %q", out)
	}
	if out := captureStdout(t, func() { _ = runPreToolUseClaim(f.cfg, hookInput{Event: eventPreToolUse, ToolName: "Bash"}) }); out != "" {
		t.Fatalf("no session id means nothing to claim, got %q", out)
	}
}

func TestPermissionRequestHonoursPendingRetryWithoutReevaluating(t *testing.T) {
	t.Setenv("PILOT_HOME", t.TempDir())
	f := newFakeServe(t)
	h := hookInput{Event: eventPermissionRequest, ToolName: "Bash", ToolInput: `{"command":"terraform apply tfplan"}`, SessionID: "sess-1"}

	handled, _ := handlePermissionRequestRetry(f.cfg, h)
	if handled {
		t.Fatal("a fresh call must fall through to evaluation")
	}

	f.claim = map[string]any{"decision": "ask", "reason": "no human answer"}
	var handledAsk bool
	out := captureStdout(t, func() { handledAsk, _ = handlePermissionRequestRetry(f.cfg, h) })
	if !handledAsk || out != "" {
		t.Fatalf("a pending ask must stand aside silently so Claude prompts the user; handled=%v out=%q", handledAsk, out)
	}

	f.claim = map[string]any{"decision": "allow", "reason": "approved on the Pilot dashboard"}
	out = captureStdout(t, func() { handledAsk, _ = handlePermissionRequestRetry(f.cfg, h) })
	if !handledAsk || !strings.Contains(out, `"behavior":"allow"`) {
		t.Fatalf("a pending allow must answer the prompt; handled=%v out=%q", handledAsk, out)
	}
	if f.evalCalls != 0 {
		t.Fatalf("retry decisions must not re-run evaluation, evaluate called %d times", f.evalCalls)
	}
}

func TestStopHookNudgesModelBackToDecidedRetry(t *testing.T) {
	t.Setenv("PILOT_HOME", t.TempDir())
	f := newFakeServe(t)

	if nudged := nudgeClassifierRetry(f.cfg, map[string]any{"session_id": "sess-1"}); nudged {
		t.Fatal("no pending retry must not block the stop")
	}
	if nudged := nudgeClassifierRetry(f.cfg, map[string]any{}); nudged {
		t.Fatal("no session id must not block the stop")
	}

	f.nudge = map[string]any{"decision": "allow", "reason": "matched pilot rule", "tool_name": "Bash", "tool_input": `{"command":"terraform apply tfplan"}`}
	var nudged bool
	out := captureStdout(t, func() { nudged = nudgeClassifierRetry(f.cfg, map[string]any{"session_id": "sess-1"}) })
	if !nudged || !strings.Contains(out, `"decision":"block"`) || !strings.Contains(out, "terraform apply tfplan") || !strings.Contains(out, "Retry that exact tool call now") {
		t.Fatalf("expected a block with a retry instruction, got nudged=%v out=%q", nudged, out)
	}

	f.nudge = map[string]any{"decision": "ask", "reason": "timeout", "tool_name": "Bash", "tool_input": `{"command":"terraform destroy"}`}
	out = captureStdout(t, func() { nudged = nudgeClassifierRetry(f.cfg, map[string]any{"session_id": "sess-1"}) })
	if !nudged || !strings.Contains(out, "user is prompted") {
		t.Fatalf("an ask retry should tell the model the user will be prompted, got %q", out)
	}
}

func TestParseHookInputReadsDenialFields(t *testing.T) {
	raw := `{"session_id":"s","cwd":"/w","hook_event_name":"PermissionDenied","tool_name":"Bash","tool_input":{"command":"x"},"tool_use_id":"t","reason":"Blocked by classifier"}`
	h := parseHookInput([]byte(raw), eventPermissionRequest)
	if h.Event != eventPermissionDenied || h.Reason != "Blocked by classifier" || h.SessionID != "s" || h.ToolInput != `{"command":"x"}` {
		t.Fatalf("unexpected parse: %+v", h)
	}
	if got := parseHookInput([]byte(`{"tool_name":"Bash"}`), eventPreToolUse); got.Event != eventPreToolUse {
		t.Fatalf("missing event must fall back to the command's default, got %q", got.Event)
	}
}

func TestPermissionDeniedHeadlessSessionDoesNotRetryIntoAPrompt(t *testing.T) {
	t.Setenv("PILOT_HOME", t.TempDir())
	t.Setenv("CLAUDE_CODE_ENTRYPOINT", "sdk-cli") // what `claude -p` hands its hooks
	f := newFakeServe(t)
	f.cfg.General.EscalationTimeoutS = 0

	out := captureStdout(t, func() {
		res := &evalResult{Decision: "deny", Reason: "gh pr merge is a human decision", Source: "haiku"}
		if err := handlePermissionDeniedResult(f.cfg, deniedInput(), res, time.Now()); err != nil {
			t.Fatal(err)
		}
	})
	if out != "" || len(f.registered()) != 0 {
		t.Fatalf("headless has no prompt to route to; the denial must stand. out=%q regs=%v", out, f.registered())
	}

	// An approval is still worth a retry: PreToolUse "allow" needs no prompt.
	out = captureStdout(t, func() {
		res := &evalResult{Decision: "approve", Reason: "matched pilot rule", Source: "pilot_rules"}
		if err := handlePermissionDeniedResult(f.cfg, deniedInput(), res, time.Now()); err != nil {
			t.Fatal(err)
		}
	})
	if !strings.Contains(out, `"retry":true`) {
		t.Fatalf("headless approvals must still retry, got %q", out)
	}
}

func TestPostServeSendsBearerWhenTokenConfigured(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
	}))
	t.Cleanup(srv.Close)
	_, portStr, _ := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))
	var port int
	for _, c := range portStr {
		port = port*10 + int(c-'0')
	}
	cfg := &config.PilotConfig{}
	cfg.General.SSEPort = port

	t.Setenv("PILOT_SERVER_TOKEN", "")
	home := t.TempDir()
	t.Setenv("PILOT_HOME", home)
	resp, err := postServe(cfg, "/internal/retry", []byte(`{}`), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if gotAuth != "" {
		t.Fatalf("no token configured, but header sent: %q", gotAuth)
	}

	// The token in ~/.pilot/.env — the file the server reads — must reach hooks
	// that Claude Code spawns without the server's environment.
	fileToken, envToken := "from-env-file", "from-process-env"
	if err := os.WriteFile(home+"/.env", []byte("ANTHROPIC_API_KEY=sk-test\nPILOT_SERVER_TOKEN=\""+fileToken+"\"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	resp, err = postServe(cfg, "/internal/retry", []byte(`{}`), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if want := "Bearer " + fileToken; gotAuth != want {
		t.Fatalf("expected bearer from .env, got %q", gotAuth)
	}

	t.Setenv("PILOT_SERVER_TOKEN", envToken)
	resp, err = postServe(cfg, "/internal/retry", []byte(`{}`), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if want := "Bearer " + envToken; gotAuth != want {
		t.Fatalf("process environment must win over .env, got %q", gotAuth)
	}
}
