package approve

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCheckCodexSettingsTrustedProject(t *testing.T) {
	home := t.TempDir()
	projectDir := filepath.Join(home, "work", "trusted")
	if err := os.MkdirAll(filepath.Join(home, ".codex"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(projectDir, 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)

	config := `[projects."` + projectDir + `"]
trust_level = "trusted"
`
	if err := os.WriteFile(filepath.Join(home, ".codex", "config.toml"), []byte(config), 0644); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name      string
		toolName  string
		toolInput string
		want      string
	}{
		{"bash safe", "Bash", `{"command":"git status --short"}`, "allow"},
		{"bash destructive", "Bash", `{"command":"git reset --hard HEAD"}`, "deny"},
		{"apply patch", "apply_patch", `{"cmd":"*** Begin Patch"}`, "allow"},
		{"edit inside project", "Edit", `{"file_path":"internal/foo.go"}`, "allow"},
		{"write outside project", "Write", `{"file_path":"/tmp/outside.txt"}`, ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := CheckCodexSettings(tt.toolName, parseTool(tt.toolInput), tt.toolInput, projectDir)
			if got != tt.want {
				t.Fatalf("CheckCodexSettings(%q, %q) = %q, want %q", tt.toolName, tt.toolInput, got, tt.want)
			}
		})
	}
}

func TestCheckCodexSettingsUntrustedProject(t *testing.T) {
	home := t.TempDir()
	projectDir := filepath.Join(home, "work", "untrusted")
	if err := os.MkdirAll(filepath.Join(home, ".codex"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(projectDir, 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)

	config := `[projects."` + projectDir + `"]
trust_level = "untrusted"
`
	if err := os.WriteFile(filepath.Join(home, ".codex", "config.toml"), []byte(config), 0644); err != nil {
		t.Fatal(err)
	}

	got := CheckCodexSettings("Bash", parseTool(`{"command":"git status --short"}`), `{"command":"git status --short"}`, projectDir)
	if got != "" {
		t.Fatalf("CheckCodexSettings in untrusted project = %q, want empty", got)
	}
}
