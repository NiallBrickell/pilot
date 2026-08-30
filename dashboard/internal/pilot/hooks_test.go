package pilot

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLegacyClaudeApprovalIsNotHealthyInstall(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	claudePath := filepath.Join(home, ".claude", "settings.json")
	codexHooks := filepath.Join(home, ".codex", "hooks.json")
	codexConfig := filepath.Join(home, ".codex", "config.toml")
	if err := os.MkdirAll(filepath.Dir(claudePath), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(codexHooks), 0755); err != nil {
		t.Fatal(err)
	}
	legacy := `{"hooks":{"PreToolUse":[{"matcher":".*","hooks":[{"type":"command","command":"/tmp/pilot approve"}]}]}}`
	if err := os.WriteFile(claudePath, []byte(legacy), 0644); err != nil {
		t.Fatal(err)
	}

	if !HooksNeedRepair() {
		t.Fatal("legacy PreToolUse approval did not trigger repair")
	}
	if CheckHooksInstalled().Installed {
		t.Fatal("legacy PreToolUse approval must not count as a healthy install")
	}

	canonical := `{"hooks":{"PermissionRequest":[{"matcher":".*","hooks":[{"type":"command","command":"/tmp/pilot approve"}]}]}}`
	if err := os.WriteFile(claudePath, []byte(canonical), 0644); err != nil {
		t.Fatal(err)
	}
	codexCanonical := `{"hooks":{"PermissionRequest":[{"matcher":".*","hooks":[{"type":"command","command":"/tmp/pilot codex-approve"}]}]}}`
	if err := os.WriteFile(codexHooks, []byte(codexCanonical), 0644); err != nil {
		t.Fatal(err)
	}
	configCanonical := `approvals_reviewer = "auto_review"
[features]
hooks = true
exec_permission_approvals = true
request_permissions_tool = true

[hooks.state."/tmp/hooks.json:permission_request:0:0"]
trusted_hash = "sha256:abc123"
`
	if err := os.WriteFile(codexConfig, []byte(configCanonical), 0600); err != nil {
		t.Fatal(err)
	}
	if HooksNeedRepair() {
		t.Fatal("canonical Claude/Codex hook install was classified as stale")
	}
	if !CheckHooksInstalled().Installed {
		t.Fatal("canonical Claude/Codex install was not detected")
	}

	staleCodexConfig := strings.Replace(configCanonical, "hooks = true", "codex_hooks = true", 1)
	if err := os.WriteFile(codexConfig, []byte(staleCodexConfig), 0600); err != nil {
		t.Fatal(err)
	}
	if !HooksNeedRepair() {
		t.Fatal("stale Codex feature config did not trigger repair")
	}
}
