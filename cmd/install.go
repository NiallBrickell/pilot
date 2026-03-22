package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/NiallBrickell/pilot/internal/config"
	"github.com/NiallBrickell/pilot/internal/paths"
	"github.com/spf13/cobra"
)

func init() {
	rootCmd.AddCommand(&cobra.Command{
		Use:   "install",
		Short: "Set up ~/.pilot/ and print Claude Code hook configuration",
		RunE:  runInstall,
	})
}

func runInstall(cmd *cobra.Command, args []string) error {
	// Auto-setup: create ~/.pilot/ and default config if needed
	if err := paths.EnsureSetup(config.EmbeddedConfig()); err != nil {
		return fmt.Errorf("failed to set up %s: %w", paths.PilotDir(), err)
	}
	fmt.Printf("Config directory: %s\n\n", paths.PilotDir())

	pilotBin := findPilotBinary()
	hookConfig := generateHookConfig(pilotBin)

	configJSON, err := json.MarshalIndent(hookConfig, "", "  ")
	if err != nil {
		return err
	}

	fmt.Println("=== pilot installation ===")
	fmt.Println()
	fmt.Println("1. Add to your Claude Code settings (~/.claude/settings.json):")
	fmt.Println()
	fmt.Println(string(configJSON))
	fmt.Println()
	fmt.Println("2. Or add hooks to your project settings (.claude/settings.json):")
	fmt.Println()
	fmt.Println(string(configJSON))
	fmt.Println()
	fmt.Println("3. Ensure ANTHROPIC_API_KEY is set in your environment or in:")
	fmt.Printf("   %s\n", paths.EnvFile())
	fmt.Println()
	fmt.Printf("4. For the PTY wrapper (auto-respond to idle pauses), run:\n")
	fmt.Printf("   %s wrap [claude args...]\n", pilotBin)
	fmt.Println()
	fmt.Println("5. Optionally symlink the binary:")
	fmt.Printf("   ln -sf %s /usr/local/bin/pilot\n", pilotBin)

	return nil
}

func generateHookConfig(pilotBin string) map[string]any {
	return map[string]any{
		"hooks": map[string]any{
			"PreToolUse": []any{
				map[string]any{
					"matcher": "^(Bash|Write|Edit|NotebookEdit|WebFetch|WebSearch)$",
					"hooks": []any{
						map[string]any{
							"type":    "command",
							"command": pilotBin + " approve",
						},
					},
				},
			},
			"Stop": []any{
				map[string]any{
					"hooks": []any{
						map[string]any{
							"type":    "command",
							"command": pilotBin + " on-stop",
						},
					},
				},
			},
		},
	}
}

func findPilotBinary() string {
	// First: try the running binary's own path
	exe, err := os.Executable()
	if err == nil {
		resolved, err := filepath.EvalSymlinks(exe)
		if err == nil {
			return resolved
		}
		return exe
	}
	// Fallback: check PATH
	if p, err := exec.LookPath("pilot"); err == nil {
		return p
	}
	return "pilot"
}
