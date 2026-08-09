package state

import (
	"os"
	"strings"
	"testing"
	"time"
)

// TestMain points PILOT_HOME at a throwaway dir so the package's tests hit a
// temp pilot.db rather than the developer's real one. getDB opens the database
// exactly once per process, so this must run before any recording call.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "pilot-state-test-")
	if err != nil {
		panic(err)
	}
	os.Setenv("PILOT_HOME", dir)
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

// TestRecordActionRedactsSecrets proves the capture-time scrub: a tool call
// carrying an inline password — the exact shape that once leaked into a public
// commit — is stored redacted, and the surrounding command structure survives.
func TestRecordActionRedactsSecrets(t *testing.T) {
	secret := "wLraSecretPass2xKpq7yQnBve5tZQ"
	toolInput := `{"command":"PGPASSWORD='` + secret + `' psql \"postgresql://claude_code:` + secret + `@203.0.113.71:5432/db\" -c '\\dt'"}`

	err := RecordAction(PilotAction{
		Timestamp:  time.Now().UTC(),
		ActionType: AutoApprove,
		Detail:     "Bash: PGPASSWORD='" + secret + "' psql ...",
		ToolName:   "Bash",
		ToolInput:  toolInput,
		Cwd:        "/tmp",
	})
	if err != nil {
		t.Fatalf("RecordAction: %v", err)
	}

	var detail, storedInput string
	row := getDB().QueryRow(
		`SELECT detail, tool_input FROM actions ORDER BY id DESC LIMIT 1`)
	if err := row.Scan(&detail, &storedInput); err != nil {
		t.Fatalf("scan: %v", err)
	}

	if strings.Contains(detail, secret) {
		t.Errorf("detail still contains secret: %q", detail)
	}
	if strings.Contains(storedInput, secret) {
		t.Errorf("tool_input still contains secret: %q", storedInput)
	}
	if !strings.Contains(storedInput, "***REDACTED***") {
		t.Errorf("tool_input missing redaction placeholder: %q", storedInput)
	}
	// Structure must survive so the fixture stays meaningful.
	if !strings.Contains(storedInput, "psql") || !strings.Contains(storedInput, "203.0.113.71:5432/db") {
		t.Errorf("tool_input lost command structure: %q", storedInput)
	}
}

// TestWriteLogRedactsSecrets proves the log path shares the same scrub.
func TestWriteLogRedactsSecrets(t *testing.T) {
	secret := "topsecretbearer0123456789"
	WriteLog("debug", "test", `curl -H "Authorization: Bearer `+secret+`" https://api.example.com`)

	logs := ReadLogs(5)
	for _, l := range logs {
		if strings.Contains(l.Message, secret) {
			t.Errorf("log message still contains secret: %q", l.Message)
		}
	}
}
