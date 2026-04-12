package cmd

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/NiallBrickell/pilot/internal/anthropic"
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
	paths.RecordBinaryPath()

	// Preflight: pilot serve will exit immediately without an API key, which
	// would cause silent toggle failures in the dashboard. Catch it here.
	if anthropic.ResolveAPIKey(paths.EnvFile()) == "" {
		return fmt.Errorf("ANTHROPIC_API_KEY not set\n\nSet it in your shell or write it to %s, e.g.:\n  echo 'ANTHROPIC_API_KEY=sk-ant-...' > %s", paths.EnvFile(), paths.EnvFile())
	}

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

	// Start serve in background and capture stderr so we can report startup
	// failures back to the user instead of dying silently.
	logPath := filepath.Join(paths.PilotDir(), "pilot-serve.log")
	logFile, _ := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
	serveCmd := exec.Command(pilotBin, "serve")
	serveCmd.Stdout = logFile
	serveCmd.Stderr = logFile
	serveCmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	if err := serveCmd.Start(); err != nil {
		return fmt.Errorf("failed to start pilot serve: %w", err)
	}

	pidPath := paths.ServePidFile()
	os.WriteFile(pidPath, []byte(fmt.Sprintf("%d", serveCmd.Process.Pid)), 0644)
	go serveCmd.Wait()

	// Verify server actually came up rather than dying immediately.
	port := cfg.General.SSEPort
	if port == 0 {
		port = 9721
	}
	if err := waitForServe(port, 3*time.Second); err != nil {
		// Surface log contents so the user sees the real reason.
		logData, _ := os.ReadFile(logPath)
		return fmt.Errorf("pilot serve failed to start: %w\n\nServer log:\n%s", err, string(logData))
	}

	fmt.Printf("Server started (pid %d)\n", serveCmd.Process.Pid)
	fmt.Println("Pilot is running")
	return nil
}

func waitForServe(port int, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	client := &http.Client{Timeout: 200 * time.Millisecond}
	url := fmt.Sprintf("http://localhost:%d/status", port)
	for time.Now().Before(deadline) {
		resp, err := client.Get(url)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("server did not respond on port %d within %s", port, timeout)
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
			if isPilotHookEntry(string(entryJSON)) {
				continue // Replace any pre-existing pilot entry
			}
			result = append(result, entry)
		}
	}

	for _, entry := range pilotEntries {
		result = append(result, entry)
	}
	return result
}

// isPilotHookEntry returns true if a serialized hook entry references one of
// pilot's subcommands, regardless of the binary path.
func isPilotHookEntry(entryJSON string) bool {
	return strings.Contains(entryJSON, "pilot approve") ||
		strings.Contains(entryJSON, "pilot interrogate") ||
		strings.Contains(entryJSON, "pilot on-stop")
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

	// Remove pilot entries from PreToolUse and Stop. Match by command name
	// (pilot approve / interrogate / on-stop) rather than by binary path so
	// we catch entries installed via a different path (e.g. ~/.pilot/pilot
	// symlink vs the working-dir build).
	for _, key := range []string{"PreToolUse", "Stop"} {
		arr, ok := hooks[key].([]any)
		if !ok {
			continue
		}
		var filtered []any
		for _, entry := range arr {
			entryJSON, _ := json.Marshal(entry)
			if !isPilotHookEntry(string(entryJSON)) {
				filtered = append(filtered, entry)
			}
		}
		if len(filtered) == 0 {
			delete(hooks, key)
		} else {
			hooks[key] = filtered
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
