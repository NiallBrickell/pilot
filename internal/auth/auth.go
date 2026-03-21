package auth

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/NiallBrickell/pilot/internal/paths"
)

func cachePath() string {
	return paths.AuthCache()
}

// CheckClaudeAuth fails fast with a clear error for interactive commands.
func CheckClaudeAuth() error {
	if !IsClaudeAuthed() {
		return fmt.Errorf("Claude is not authenticated. Run 'claude auth login' first.")
	}
	return nil
}

// IsClaudeAuthed is a silent auth check for hooks — returns false instead of erroring.
// Caches result in a file for 1 hour.
func IsClaudeAuthed() bool {
	path := cachePath()

	// Check cache (valid for 1 hour)
	if info, err := os.Stat(path); err == nil {
		if time.Since(info.ModTime()) < time.Hour {
			if content, err := os.ReadFile(path); err == nil {
				return strings.TrimSpace(string(content)) == "true"
			}
		}
	}

	// Run claude auth status
	cmd := exec.Command("claude", "auth", "status")
	out, err := cmd.Output()
	authed := false
	if err == nil {
		stdout := string(out)
		authed = strings.Contains(stdout, `"loggedIn": true`) || strings.Contains(stdout, `"loggedIn":true`)
	}

	// Cache the result
	val := "false"
	if authed {
		val = "true"
	}
	_ = os.WriteFile(path, []byte(val), 0644)

	return authed
}
