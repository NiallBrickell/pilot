package cmd

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/NiallBrickell/pilot/internal/config"
	"github.com/NiallBrickell/pilot/internal/paths"
	"github.com/spf13/cobra"
)

func init() {
	rootCmd.AddCommand(&cobra.Command{
		Use:   "start",
		Short: "Install hooks, start the server, and enable pilot",
		RunE:  runStart,
	})
	rootCmd.AddCommand(&cobra.Command{
		Use:   "stop",
		Short: "Remove hooks, stop the server, and disable pilot",
		RunE:  runStop,
	})
}

func runStart(cmd *cobra.Command, args []string) error {
	// Auto-setup ~/.pilot/
	if err := paths.EnsureSetup(config.EmbeddedConfig()); err != nil {
		return fmt.Errorf("failed to set up %s: %w", paths.PilotDir(), err)
	}

	pilotBin := findPilotBinary()

	// Install hooks into ~/.claude/settings.json
	if err := installHooks(pilotBin); err != nil {
		return fmt.Errorf("failed to install hooks: %w", err)
	}
	fmt.Println("Hooks installed")

	// Kill any stale serve processes
	cfg := config.Load()
	killPort(cfg.General.SSEPort)

	// Stop existing serve if running
	stopServeProcess()

	// Start serve in background
	serveCmd := exec.Command(pilotBin, "serve")
	serveCmd.Stdout = nil
	serveCmd.Stderr = nil
	serveCmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	if err := serveCmd.Start(); err != nil {
		return fmt.Errorf("failed to start pilot serve: %w", err)
	}

	pidPath := paths.ServePidFile()
	os.WriteFile(pidPath, []byte(fmt.Sprintf("%d", serveCmd.Process.Pid)), 0644)
	go serveCmd.Wait()

	fmt.Printf("Server started (pid %d)\n", serveCmd.Process.Pid)
	fmt.Println("Pilot is running")
	return nil
}

func runStop(cmd *cobra.Command, args []string) error {
	// Remove hooks from ~/.claude/settings.json
	if err := uninstallHooks(); err != nil {
		slog.Warn("Failed to remove hooks", "error", err)
	} else {
		fmt.Println("Hooks removed")
	}

	// Stop serve process
	stopServeProcess()

	// Kill any stale processes
	cfg := config.Load()
	killPort(cfg.General.SSEPort)

	exec.Command("pkill", "-f", "pilot approve").Run()
	exec.Command("pkill", "-f", "pilot interrogate").Run()
	exec.Command("pkill", "-f", "pilot on-stop").Run()

	fmt.Println("Pilot stopped")
	return nil
}

// --- hooks ---

func claudeSettingsPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".claude", "settings.json")
}

func installHooks(pilotBin string) error {
	path := claudeSettingsPath()

	settings := make(map[string]any)
	if data, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(data, &settings)
	}

	hooks, _ := settings["hooks"].(map[string]any)
	if hooks == nil {
		hooks = make(map[string]any)
	}

	pilotPreToolUse := map[string]any{
		"matcher": "^(Bash|Write|Edit|NotebookEdit|WebFetch|WebSearch|Read|Grep|Glob|Agent)$",
		"hooks": []any{
			map[string]any{
				"type":    "command",
				"command": pilotBin + " approve",
			},
		},
	}

	pilotInterrogate := map[string]any{
		"matcher": ".*",
		"hooks": []any{
			map[string]any{
				"type":    "command",
				"command": pilotBin + " interrogate",
			},
		},
	}

	pilotStop := map[string]any{
		"hooks": []any{
			map[string]any{
				"type":    "command",
				"command": pilotBin + " on-stop",
			},
		},
	}

	// For each hook type: keep existing non-pilot entries, replace/add pilot entries
	hooks["PreToolUse"] = mergeHookEntries(hooks["PreToolUse"], pilotBin, pilotPreToolUse, pilotInterrogate)
	hooks["Stop"] = mergeHookEntries(hooks["Stop"], pilotBin, pilotStop)

	settings["hooks"] = hooks

	data, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return err
	}

	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}

	return os.WriteFile(path, data, 0644)
}

// mergeHookEntries keeps existing non-pilot hook entries and adds/replaces pilot entries.
func mergeHookEntries(existing any, pilotBin string, pilotEntries ...map[string]any) []any {
	var result []any

	if arr, ok := existing.([]any); ok {
		for _, entry := range arr {
			entryJSON, _ := json.Marshal(entry)
			s := string(entryJSON)
			if strings.Contains(s, pilotBin) {
				continue // Remove old pilot entry, we'll add the new ones
			}
			if strings.Contains(s, "pilot approve") || strings.Contains(s, "pilot interrogate") || strings.Contains(s, "pilot on-stop") {
				// Old pilot binary at a different path — warn and skip
				fmt.Fprintf(os.Stderr, "Warning: replacing existing pilot hook entry (old path)\n")
				continue
			}
			result = append(result, entry)
		}
	}

	for _, entry := range pilotEntries {
		result = append(result, entry)
	}
	return result
}

func uninstallHooks() error {
	path := claudeSettingsPath()

	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}

	settings := make(map[string]any)
	if err := json.Unmarshal(data, &settings); err != nil {
		return err
	}

	hooks, _ := settings["hooks"].(map[string]any)
	if hooks == nil {
		return nil
	}

	bin := findPilotBinary()

	// Remove pilot entries from PreToolUse
	if pre, ok := hooks["PreToolUse"].([]any); ok {
		var filtered []any
		for _, entry := range pre {
			entryJSON, _ := json.Marshal(entry)
			if !strings.Contains(string(entryJSON), bin) {
				filtered = append(filtered, entry)
			}
		}
		if len(filtered) == 0 {
			delete(hooks, "PreToolUse")
		} else {
			hooks["PreToolUse"] = filtered
		}
	}

	// Remove pilot entries from Stop
	if stop, ok := hooks["Stop"].([]any); ok {
		var filtered []any
		for _, entry := range stop {
			entryJSON, _ := json.Marshal(entry)
			if !strings.Contains(string(entryJSON), bin) {
				filtered = append(filtered, entry)
			}
		}
		if len(filtered) == 0 {
			delete(hooks, "Stop")
		} else {
			hooks["Stop"] = filtered
		}
	}

	if len(hooks) == 0 {
		delete(settings, "hooks")
	} else {
		settings["hooks"] = hooks
	}

	out, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return err
	}

	return os.WriteFile(path, out, 0644)
}

// --- process management ---

func stopServeProcess() {
	pidPath := paths.ServePidFile()
	data, err := os.ReadFile(pidPath)
	if err != nil {
		return
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		os.Remove(pidPath)
		return
	}
	syscall.Kill(pid, syscall.SIGTERM)
	os.Remove(pidPath)
}

func killPort(port int) {
	if port == 0 {
		return
	}
	out, err := exec.Command("lsof", fmt.Sprintf("-ti:%d", port)).Output()
	if err != nil || len(out) == 0 {
		return
	}
	for _, pidStr := range strings.Fields(strings.TrimSpace(string(out))) {
		if pid, err := strconv.Atoi(pidStr); err == nil {
			syscall.Kill(pid, syscall.SIGKILL)
		}
	}
}

func findPilotBinary() string {
	exe, err := os.Executable()
	if err == nil {
		resolved, err := filepath.EvalSymlinks(exe)
		if err == nil {
			return resolved
		}
		return exe
	}
	if p, err := exec.LookPath("pilot"); err == nil {
		return p
	}
	return "pilot"
}
