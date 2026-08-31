package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/NiallBrickell/pilot/internal/config"
)

func TestRetryKeyIgnoresBashDescriptionOnly(t *testing.T) {
	a := RetryKey("s1", "Bash", `{"command":"terraform apply plan.out","description":"Apply prod plan"}`)
	b := RetryKey("s1", "Bash", `{"description":"Apply the plan (retry)","command":"terraform apply plan.out"}`)
	if a != b {
		t.Fatal("a retry that only rewrites the description must match the original call")
	}
	if RetryKey("s1", "Bash", `{"command":"terraform apply other.out"}`) == a {
		t.Fatal("a different command must not match")
	}
	if RetryKey("s2", "Bash", `{"command":"terraform apply plan.out"}`) == a {
		t.Fatal("another session must not match")
	}
	if RetryKey("s1", "Edit", `{"command":"terraform apply plan.out"}`) == a {
		t.Fatal("another tool must not match")
	}
	// Non-JSON input still produces a stable key.
	if RetryKey("s1", "Bash", "raw") != RetryKey("s1", "Bash", "raw") {
		t.Fatal("raw input key must be stable")
	}
}

func TestRetryStoreAllowIsConsumedByPreToolUse(t *testing.T) {
	rs := NewRetryStore()
	rs.Register("s1", "Bash", `{"command":"gh pr create"}`, RetryAllow, "matched pilot rule")

	rec := rs.Claim("s1", "Bash", `{"command":"gh pr create","description":"open PR"}`, RetryStagePreToolUse)
	if rec == nil || rec.Decision != RetryAllow {
		t.Fatalf("expected allow on retry, got %+v", rec)
	}
	if rs.Claim("s1", "Bash", `{"command":"gh pr create"}`, RetryStagePreToolUse) != nil {
		t.Fatal("an allow must be one-shot")
	}
	if rs.Claim("s1", "Bash", `{"command":"gh pr create"}`, RetryStagePermissionRequest) != nil {
		t.Fatal("a consumed allow must not leak into PermissionRequest")
	}
}

func TestRetryStoreAskSurvivesUntilPermissionRequest(t *testing.T) {
	rs := NewRetryStore()
	rs.Register("s1", "Bash", `{"command":"terraform destroy"}`, RetryAsk, "no human answer before timeout")

	pre := rs.Claim("s1", "Bash", `{"command":"terraform destroy"}`, RetryStagePreToolUse)
	if pre == nil || pre.Decision != RetryAsk {
		t.Fatalf("PreToolUse should be told to ask, got %+v", pre)
	}
	// The PermissionRequest hook fires for the same call right after the
	// PreToolUse "ask"; it must still find the record so it can stand aside.
	pr := rs.Claim("s1", "Bash", `{"command":"terraform destroy"}`, RetryStagePermissionRequest)
	if pr == nil || pr.Decision != RetryAsk {
		t.Fatalf("PermissionRequest should see the ask, got %+v", pr)
	}
	if rs.Claim("s1", "Bash", `{"command":"terraform destroy"}`, RetryStagePermissionRequest) != nil {
		t.Fatal("PermissionRequest must consume the record")
	}
}

func TestRetryStoreNudgeOncePerRecordAndSkipsPrompted(t *testing.T) {
	rs := NewRetryStore()
	base := time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC)
	rs.now = func() time.Time { return base }
	rs.Register("s1", "Bash", `{"command":"b"}`, RetryAllow, "")
	rs.now = func() time.Time { return base.Add(time.Second) }
	rs.Register("s1", "Bash", `{"command":"a"}`, RetryAllow, "")
	rs.Register("s2", "Bash", `{"command":"c"}`, RetryAllow, "")

	first := rs.Nudge("s1")
	if first == nil || first.ToolInput != `{"command":"b"}` {
		t.Fatalf("oldest unclaimed record should be nudged first, got %+v", first)
	}
	second := rs.Nudge("s1")
	if second == nil || second.ToolInput != `{"command":"a"}` {
		t.Fatalf("next record should be nudged next, got %+v", second)
	}
	if rs.Nudge("s1") != nil {
		t.Fatal("each record nudges at most once")
	}
	if rs.Nudge("s3") != nil {
		t.Fatal("unknown session must not be nudged")
	}

	rs.Register("s2", "Bash", `{"command":"d"}`, RetryAsk, "")
	rs.Claim("s2", "Bash", `{"command":"d"}`, RetryStagePreToolUse)
	got := rs.Nudge("s2")
	if got == nil || got.ToolInput != `{"command":"c"}` {
		t.Fatalf("a record with a prompt in flight must not be nudged, got %+v", got)
	}
}

func TestRetryStoreExpiresAndCaps(t *testing.T) {
	rs := NewRetryStore()
	base := time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC)
	rs.now = func() time.Time { return base }
	rs.Register("s1", "Bash", `{"command":"old"}`, RetryAllow, "")
	rs.now = func() time.Time { return base.Add(retryTTL + time.Second) }
	if rs.Claim("s1", "Bash", `{"command":"old"}`, RetryStagePreToolUse) != nil {
		t.Fatal("expired record must not be claimable")
	}

	for i := 0; i < retryMaxRecords+5; i++ {
		rs.now = func() time.Time { return base.Add(retryTTL + 2*time.Second + time.Duration(i)*time.Millisecond) }
		rs.Register("s", "Bash", string(rune('a'+i%26))+string(rune(i)), RetryAllow, "")
	}
	if rs.Len() > retryMaxRecords {
		t.Fatalf("store must cap at %d records, has %d", retryMaxRecords, rs.Len())
	}
}

func TestRetryEndpointRegisterClaimNudge(t *testing.T) {
	t.Setenv("PILOT_HOME", t.TempDir()) // never read the developer's real ~/.pilot/.env
	t.Setenv("PILOT_SERVER_TOKEN", "")
	srv := New(&config.PilotConfig{})
	handler := srv.handler()
	call := func(body map[string]any) map[string]any {
		t.Helper()
		b, _ := json.Marshal(body)
		req := httptest.NewRequest(http.MethodPost, "/internal/retry", bytes.NewReader(b))
		res := httptest.NewRecorder()
		handler.ServeHTTP(res, req)
		if res.Code != http.StatusOK {
			t.Fatalf("status %d: %s", res.Code, res.Body.String())
		}
		var out map[string]any
		if err := json.Unmarshal(res.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		return out
	}

	out := call(map[string]any{"action": "register", "session_id": "s1", "tool_name": "Bash",
		"tool_input": `{"command":"gh pr create","description":"x"}`, "decision": "allow", "reason": "matched pilot rule"})
	if out["registered"] != true {
		t.Fatalf("register: %v", out)
	}
	if out := call(map[string]any{"action": "nudge", "session_id": "s1"}); out["decision"] != "allow" {
		t.Fatalf("nudge should surface the unclaimed record: %v", out)
	}
	out = call(map[string]any{"action": "claim", "stage": RetryStagePreToolUse, "session_id": "s1", "tool_name": "Bash",
		"tool_input": `{"command":"gh pr create","description":"retry"}`})
	if out["decision"] != "allow" || out["reason"] != "matched pilot rule" {
		t.Fatalf("claim: %v", out)
	}
	out = call(map[string]any{"action": "claim", "stage": RetryStagePreToolUse, "session_id": "s1", "tool_name": "Bash",
		"tool_input": `{"command":"gh pr create"}`})
	if out["decision"] != "" {
		t.Fatalf("second claim must find nothing: %v", out)
	}

	req := httptest.NewRequest(http.MethodPost, "/internal/retry", bytes.NewReader([]byte(`{"action":"bogus"}`)))
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, req)
	if res.Code != http.StatusBadRequest {
		t.Fatalf("unknown action should be rejected, got %d", res.Code)
	}
	req = httptest.NewRequest(http.MethodPost, "/internal/retry", bytes.NewReader([]byte(`{"action":"register","session_id":"s","tool_name":"Bash","tool_input":"{}","decision":"maybe"}`)))
	res = httptest.NewRecorder()
	handler.ServeHTTP(res, req)
	if res.Code != http.StatusBadRequest {
		t.Fatalf("unknown decision should be rejected, got %d", res.Code)
	}
}

func TestNewLoadsServerTokenFromEnvFile(t *testing.T) {
	t.Setenv("PILOT_SERVER_TOKEN", "")
	home := t.TempDir()
	t.Setenv("PILOT_HOME", home)
	if got := New(&config.PilotConfig{}).serverToken; got != "" {
		t.Fatalf("no token anywhere, got %q", got)
	}
	if err := os.WriteFile(home+"/.env", []byte("PILOT_SERVER_TOKEN=file-token\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if got := New(&config.PilotConfig{}).serverToken; got != "file-token" {
		t.Fatalf("token from ~/.pilot/.env not loaded, got %q", got)
	}
}
