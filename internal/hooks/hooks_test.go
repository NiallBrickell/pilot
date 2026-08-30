package hooks

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func TestInstallAllAddsClaudeAndCodexHooks(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	configPath := filepath.Join(home, "pilot.toml")
	t.Setenv("PILOT_CONFIG", configPath)
	if err := os.WriteFile(configPath, []byte("[general]\ninterrogation_enabled = true\n"), 0644); err != nil {
		t.Fatal(err)
	}

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
	if !strings.Contains(string(configData), "[features]") || !strings.Contains(string(configData), "hooks = true") {
		t.Fatalf("Codex feature flag not enabled:\n%s", configData)
	}
	if strings.Contains(string(configData), "codex_hooks") {
		t.Fatalf("Deprecated codex_hooks flag should be removed:\n%s", configData)
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

// TestClaudeApprovalRunsOnlyOnPermissionRequest pins the ordering that keeps
// Claude auto mode in front of Pilot. PreToolUse fires before Claude's own
// permission classifier; PermissionRequest fires only when Claude needs an
// external decision. The catch-all matcher still covers tools added later.
func TestClaudeApprovalRunsOnlyOnPermissionRequest(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("PILOT_CONFIG", filepath.Join(home, "pilot.toml"))

	if err := InstallClaude("/tmp/pilot"); err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(filepath.Join(home, ".claude", "settings.json"))
	if err != nil {
		t.Fatal(err)
	}
	var settings struct {
		Hooks struct {
			PreToolUse []struct {
				Matcher string `json:"matcher"`
				Hooks   []struct {
					Command string `json:"command"`
				} `json:"hooks"`
			} `json:"PreToolUse"`
			PermissionRequest []struct {
				Matcher string `json:"matcher"`
				Hooks   []struct {
					Command string `json:"command"`
				} `json:"hooks"`
			} `json:"PermissionRequest"`
		} `json:"hooks"`
	}
	if err := json.Unmarshal(data, &settings); err != nil {
		t.Fatal(err)
	}

	var matcher string
	for _, entry := range settings.Hooks.PermissionRequest {
		for _, h := range entry.Hooks {
			if strings.HasSuffix(h.Command, "pilot approve") {
				matcher = entry.Matcher
			}
		}
	}
	if matcher == "" {
		t.Fatalf("no Pilot PermissionRequest approval hook installed:\n%s", data)
	}
	for _, entry := range settings.Hooks.PreToolUse {
		for _, h := range entry.Hooks {
			if strings.HasSuffix(h.Command, "pilot approve") {
				t.Fatalf("Pilot approval must not run before Claude auto mode:\n%s", data)
			}
		}
	}

	re, err := regexp.Compile(matcher)
	if err != nil {
		t.Fatalf("matcher %q does not compile: %v", matcher, err)
	}
	for _, tool := range []string{
		"Bash", "Read", "Edit", "Agent", "mcp__axiom__queryDataset",
		"Monitor", "Skill", "ToolSearch", "TaskCreate", "TaskUpdate",
		"Artifact", "RemoteTrigger", "SendMessage", "SendUserFile",
		"Workflow", "CronCreate", "EnterWorktree", "ScheduleWakeup",
		"SomeToolClaudeCodeHasNotShippedYet",
	} {
		if !re.MatchString(tool) {
			t.Errorf("matcher %q does not match %q — the hook would never run and the user gets the prompt", matcher, tool)
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

	claudeData, err := os.ReadFile(filepath.Join(home, ".claude", "settings.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(claudeData), "pilot approve") {
		t.Fatalf("Claude Pilot approval hook not removed:\n%s", claudeData)
	}
}

func TestInstallCodexOmitsStopHookWhenRepliesDisabled(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	configPath := filepath.Join(home, "pilot.toml")
	t.Setenv("PILOT_CONFIG", configPath)
	if err := os.WriteFile(configPath, []byte("[general]\nstop_hook_replies = false\ninterrogation_enabled = true\n"), 0644); err != nil {
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

func TestInstallCodexSetsApprovalsReviewer(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	configPath := filepath.Join(home, "pilot.toml")
	t.Setenv("PILOT_CONFIG", configPath)
	if err := os.WriteFile(configPath, []byte("[general]\ninterrogation_enabled = false\n"), 0644); err != nil {
		t.Fatal(err)
	}

	// Pre-existing Codex config with a top-level key and a table, mirroring a
	// real ~/.codex/config.toml so we verify both are preserved.
	codexConfig := "model = \"gpt-5.6-sol\"\n\n[projects.\"/tmp/x\"]\ntrust_level = \"trusted\"\n"
	if err := os.MkdirAll(filepath.Join(home, ".codex"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".codex", "config.toml"), []byte(codexConfig), 0644); err != nil {
		t.Fatal(err)
	}

	if err := InstallCodex("/tmp/pilot"); err != nil {
		t.Fatal(err)
	}

	read := func() string {
		data, err := os.ReadFile(filepath.Join(home, ".codex", "config.toml"))
		if err != nil {
			t.Fatal(err)
		}
		return string(data)
	}

	got := read()
	if !strings.Contains(got, `approvals_reviewer = "auto_review"`) {
		t.Fatalf("approvals_reviewer not set:\n%s", got)
	}
	if !strings.Contains(got, `model = "gpt-5.6-sol"`) {
		t.Fatalf("existing top-level key not preserved:\n%s", got)
	}
	if !strings.Contains(got, `[projects."/tmp/x"]`) {
		t.Fatalf("existing table not preserved:\n%s", got)
	}
	// The key must sit above the first table header (valid TOML).
	if idx := strings.Index(got, "approvals_reviewer"); idx == -1 || idx > strings.Index(got, "[projects") {
		t.Fatalf("approvals_reviewer must precede the first table header:\n%s", got)
	}

	// Idempotent: a second install must not duplicate the key.
	if err := InstallCodex("/tmp/pilot"); err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(read(), "approvals_reviewer"); n != 1 {
		t.Fatalf("approvals_reviewer written %d times, want 1:\n%s", n, read())
	}
}

func TestInstallAllOmitsInterrogationHooksWhenDisabled(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	configPath := filepath.Join(home, "pilot.toml")
	t.Setenv("PILOT_CONFIG", configPath)
	if err := os.WriteFile(configPath, []byte("[general]\ninterrogation_enabled = false\n"), 0644); err != nil {
		t.Fatal(err)
	}

	if err := InstallAll("/tmp/pilot"); err != nil {
		t.Fatal(err)
	}

	claudeData, err := os.ReadFile(filepath.Join(home, ".claude", "settings.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"pilot approve", "pilot on-stop"} {
		if !strings.Contains(string(claudeData), want) {
			t.Fatalf("Claude settings missing %q:\n%s", want, claudeData)
		}
	}
	if strings.Contains(string(claudeData), "pilot interrogate") {
		t.Fatalf("Claude interrogation hook should be omitted when disabled:\n%s", claudeData)
	}

	codexData, err := os.ReadFile(filepath.Join(home, ".codex", "hooks.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"pilot codex-approve", "pilot codex-on-stop"} {
		if !strings.Contains(string(codexData), want) {
			t.Fatalf("Codex hooks missing %q:\n%s", want, codexData)
		}
	}
	if strings.Contains(string(codexData), "pilot codex-interrogate") {
		t.Fatalf("Codex interrogation hook should be omitted when disabled:\n%s", codexData)
	}
	if !CheckInstalled().Installed {
		t.Fatalf("hooks should count as installed when interrogation is disabled")
	}

	configData, err := os.ReadFile(filepath.Join(home, ".codex", "config.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(configData), "pre_tool_use") {
		t.Fatalf("Codex PreToolUse trust state should be omitted when interrogation is disabled:\n%s", configData)
	}
}
