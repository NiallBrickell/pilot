package hooks

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestInstallAllAddsClaudeAndCodexHooks(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	existingCodex := map[string]any{
		"hooks": map[string]any{
			"PreToolUse": []any{
				map[string]any{
					"matcher": "Bash",
					"hooks": []any{
						map[string]any{"type": "command", "command": "/usr/bin/true"},
					},
				},
			},
		},
	}
	data, _ := json.Marshal(existingCodex)
	if err := os.MkdirAll(filepath.Join(home, ".codex"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".codex", "hooks.json"), data, 0644); err != nil {
		t.Fatal(err)
	}

	if err := InstallAll("/tmp/pilot"); err != nil {
		t.Fatal(err)
	}

	claudeData, err := os.ReadFile(filepath.Join(home, ".claude", "settings.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"pilot approve", "pilot interrogate", "pilot on-stop"} {
		if !strings.Contains(string(claudeData), want) {
			t.Fatalf("Claude settings missing %q:\n%s", want, claudeData)
		}
	}

	codexData, err := os.ReadFile(filepath.Join(home, ".codex", "hooks.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"pilot codex-approve", "pilot codex-interrogate", "pilot codex-on-stop", "/usr/bin/true"} {
		if !strings.Contains(string(codexData), want) {
			t.Fatalf("Codex hooks missing %q:\n%s", want, codexData)
		}
	}
	var codexSettings map[string]any
	if err := json.Unmarshal(codexData, &codexSettings); err != nil {
		t.Fatal(err)
	}
	hooks := codexSettings["hooks"].(map[string]any)
	preToolUse, _ := json.Marshal(hooks["PreToolUse"])
	if strings.Contains(string(preToolUse), "pilot codex-approve") {
		t.Fatalf("Codex PreToolUse must not run approval evaluation:\n%s", preToolUse)
	}
	permissionRequest, _ := json.Marshal(hooks["PermissionRequest"])
	if !strings.Contains(string(permissionRequest), "pilot codex-approve") {
		t.Fatalf("Codex PermissionRequest missing approval evaluation:\n%s", permissionRequest)
	}

	configData, err := os.ReadFile(filepath.Join(home, ".codex", "config.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(configData), "[features]") || !strings.Contains(string(configData), "codex_hooks = true") {
		t.Fatalf("Codex feature flag not enabled:\n%s", configData)
	}
	for _, want := range []string{
		"exec_permission_approvals = true",
		"request_permissions_tool = true",
	} {
		if !strings.Contains(string(configData), want) {
			t.Fatalf("Codex permission feature flag %q not enabled:\n%s", want, configData)
		}
	}
}

func TestUninstallAllRemovesOnlyPilotHooks(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	if err := InstallAll("/tmp/pilot"); err != nil {
		t.Fatal(err)
	}

	var codex map[string]any
	codexData, err := os.ReadFile(filepath.Join(home, ".codex", "hooks.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(codexData, &codex); err != nil {
		t.Fatal(err)
	}
	hooksMap := codex["hooks"].(map[string]any)
	hooksMap["PostToolUse"] = []any{
		map[string]any{
			"matcher": "Bash",
			"hooks": []any{
				map[string]any{"type": "command", "command": "/usr/bin/true"},
			},
		},
	}
	codexData, _ = json.Marshal(codex)
	if err := os.WriteFile(filepath.Join(home, ".codex", "hooks.json"), codexData, 0644); err != nil {
		t.Fatal(err)
	}

	if err := UninstallAll(); err != nil {
		t.Fatal(err)
	}

	codexData, err = os.ReadFile(filepath.Join(home, ".codex", "hooks.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(codexData), "pilot codex-") {
		t.Fatalf("Codex Pilot hooks not removed:\n%s", codexData)
	}
	if !strings.Contains(string(codexData), "/usr/bin/true") {
		t.Fatalf("non-Pilot Codex hook was removed:\n%s", codexData)
	}
}

func TestInstallCodexOmitsStopHookWhenRepliesDisabled(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	configPath := filepath.Join(home, "pilot.toml")
	t.Setenv("PILOT_CONFIG", configPath)
	if err := os.WriteFile(configPath, []byte("[general]\nstop_hook_replies = false\n"), 0644); err != nil {
		t.Fatal(err)
	}

	if err := InstallCodex("/tmp/pilot"); err != nil {
		t.Fatal(err)
	}

	codexData, err := os.ReadFile(filepath.Join(home, ".codex", "hooks.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"pilot codex-approve", "pilot codex-interrogate"} {
		if !strings.Contains(string(codexData), want) {
			t.Fatalf("Codex hooks missing %q:\n%s", want, codexData)
		}
	}
	if strings.Contains(string(codexData), "pilot codex-on-stop") {
		t.Fatalf("Codex Stop hook should be omitted when replies are disabled:\n%s", codexData)
	}
	if !CheckInstalled().CodexInstalled {
		t.Fatalf("Codex hooks should count as installed when Stop replies are disabled:\n%s", codexData)
	}
}
