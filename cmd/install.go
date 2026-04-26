package cmd

import (
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
	pilothooks "github.com/NiallBrickell/pilot/internal/hooks"
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

	if err := pilothooks.InstallAll(pilotBin); err != nil {
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
	if err := pilothooks.UninstallAll(); err != nil {
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
	exec.Command("pkill", "-f", "pilot codex-approve").Run()
	exec.Command("pkill", "-f", "pilot codex-interrogate").Run()
	exec.Command("pkill", "-f", "pilot codex-on-stop").Run()

	fmt.Println("Pilot stopped")
	return nil
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
