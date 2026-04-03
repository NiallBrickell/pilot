package approve

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func parseTool(input string) map[string]any {
	var parsed map[string]any
	if len(input) > 0 && input[0] == '{' {
		_ = json.Unmarshal([]byte(input), &parsed)
	}
	return parsed
}

func TestBuildSignature(t *testing.T) {
	tests := []struct {
		name      string
		toolName  string
		toolInput string
		want      string
	}{
		{"bash json input", "Bash", `{"command":"git status","description":"Show working tree status"}`, "Bash(git status)"},
		{"bash plain input", "Bash", "git status", "Bash(git status)"},
		{"bash json with args", "Bash", `{"command":"git diff --stat HEAD~1","description":"whatever"}`, "Bash(git diff --stat HEAD~1)"},
		{"edit json input", "Edit", `{"file_path":"/foo/bar.go","old_string":"x","new_string":"y"}`, "Edit(/foo/bar.go)"},
		{"write json input", "Write", `{"file_path":"/foo/bar.go","content":"stuff"}`, "Write(/foo/bar.go)"},
		{"plain tool no input", "WebSearch", "", "WebSearch"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := buildSignature(tt.toolName, parseTool(tt.toolInput), tt.toolInput)
			if got != tt.want {
				t.Errorf("buildSignature(%q, %q) = %q, want %q", tt.toolName, tt.toolInput, got, tt.want)
			}
		})
	}
}

func TestCheckClaudeSettings(t *testing.T) {
	// Create a temp directory structure with Claude settings
	tmpDir := t.TempDir()
	projectDir := filepath.Join(tmpDir, "myproject")
	os.MkdirAll(filepath.Join(projectDir, ".claude"), 0755)

	// Write project-level local settings that allow common operations
	// (CheckClaudeSettings walks up from cwd loading .claude/settings.local.json)
	settings := `{
		"permissions": {
			"allow": [
				"Read",
				"Glob",
				"Bash(git :*)",
				"Edit",
				"Write",
				"Bash(go build:*)",
				"Bash(npm install:*)"
			]
		}
	}`
	os.WriteFile(filepath.Join(projectDir, ".claude", "settings.local.json"), []byte(settings), 0644)

	tests := []struct {
		name      string
		toolName  string
		toolInput string
		cwd       string
		want      string
	}{
		{"builtin read", "Read", "/foo/bar.go", projectDir, "allow"},
		{"builtin glob", "Glob", "**/*.go", projectDir, "allow"},
		{"bash git status json", "Bash", `{"command":"git status","description":"Show status"}`, projectDir, "allow"},
		{"bash git status plain", "Bash", "git status", projectDir, "allow"},
		{"bash git diff json", "Bash", `{"command":"git diff --stat","description":"Show diff"}`, projectDir, "allow"},
		{"edit from project cwd", "Edit", `{"file_path":"/foo.go","old_string":"x","new_string":"y"}`, projectDir, "allow"},
		{"write from project cwd", "Write", `{"file_path":"/foo.go","content":"x"}`, projectDir, "allow"},
		{"bash go build", "Bash", `{"command":"go build ./...","description":"Build"}`, projectDir, "allow"},
		{"bash npm install", "Bash", `{"command":"npm install","description":"Install deps"}`, projectDir, "allow"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := CheckClaudeSettings(tt.toolName, parseTool(tt.toolInput), tt.toolInput, tt.cwd)
			if got != tt.want {
				t.Errorf("CheckClaudeSettings(%q, %q, %q) = %q, want %q", tt.toolName, tt.toolInput, tt.cwd, got, tt.want)
			}
		})
	}
}
