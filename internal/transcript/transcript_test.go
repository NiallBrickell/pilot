package transcript

import (
	"encoding/json"
	"testing"
)

func TestParseLineClaudeMessage(t *testing.T) {
	var entry map[string]any
	if err := json.Unmarshal([]byte(`{"type":"user","message":{"role":"user","content":[{"type":"text","text":"fix the build"}]}}`), &entry); err != nil {
		t.Fatal(err)
	}

	msg, ok := ParseLine(entry)
	if !ok {
		t.Fatal("expected message")
	}
	if msg.Role != "user" || msg.Text != "fix the build" {
		t.Fatalf("unexpected message: %#v", msg)
	}
}

func TestParseLineCodexResponseItem(t *testing.T) {
	var entry map[string]any
	if err := json.Unmarshal([]byte(`{"type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"tests pass"}]}}`), &entry); err != nil {
		t.Fatal(err)
	}

	msg, ok := ParseLine(entry)
	if !ok {
		t.Fatal("expected message")
	}
	if msg.Role != "assistant" || msg.Text != "tests pass" {
		t.Fatalf("unexpected message: %#v", msg)
	}
}

func TestParseLineCodexEventMessage(t *testing.T) {
	var entry map[string]any
	if err := json.Unmarshal([]byte(`{"type":"event_msg","payload":{"type":"user_message","message":"keep going"}}`), &entry); err != nil {
		t.Fatal(err)
	}

	msg, ok := ParseLine(entry)
	if !ok {
		t.Fatal("expected message")
	}
	if msg.Role != "user" || msg.Text != "keep going" {
		t.Fatalf("unexpected message: %#v", msg)
	}
}
