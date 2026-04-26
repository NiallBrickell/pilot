package pilot

import (
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Types shared with frontend via Wails bindings.

type PilotStatus struct {
	Available          bool          `json:"available"`
	SessionActive      bool          `json:"session_active"`
	SessionStart       *string       `json:"session_start"`
	Stats              PilotStats    `json:"stats"`
	RecentActions      []PilotAction `json:"recent_actions"`
	HasPendingResponse bool          `json:"has_pending_response"`
	HooksInstalled     bool          `json:"hooks_installed"`
	WrapperRunning     bool          `json:"wrapper_running"`
	SSEAvailable       bool          `json:"sse_available"`
	SSEPort            int           `json:"sse_port"`
}

type PilotStats struct {
	ApprovalsAuto        uint64 `json:"approvals_auto"`
	ApprovalsEscalated   uint64 `json:"approvals_escalated"`
	AutoResponses        uint64 `json:"auto_responses"`
	AutoResponsesSkipped uint64 `json:"auto_responses_skipped"`
}

type PilotAction struct {
	Timestamp  string   `json:"timestamp"`
	ActionType string   `json:"action_type"`
	Detail     string   `json:"detail"`
	Confidence *float64 `json:"confidence"`
}

// FindPilotBinary locates the pilot binary.
func FindPilotBinary() (string, error) {
	// Check ~/.pilot/pilot-bin (recorded by `pilot start` / `pilot dashboard`)
	if data, err := os.ReadFile(filepath.Join(pilotDir(), "pilot-bin")); err == nil {
		path := strings.TrimSpace(string(data))
		if path != "" {
			if _, err := os.Stat(path); err == nil {
				return path, nil
			}
		}
	}
	// Check PATH
	if p, err := exec.LookPath("pilot"); err == nil {
		return p, nil
	}
	// Check ~/.pilot/pilot
	p := filepath.Join(pilotDir(), "pilot")
	if _, err := os.Stat(p); err == nil {
		return p, nil
	}
	// Check working directory and parent (covers `wails dev` from dashboard/)
	if cwd, err := os.Getwd(); err == nil {
		for _, candidate := range []string{
			filepath.Join(cwd, "pilot"),
			filepath.Join(cwd, "..", "pilot"),
		} {
			if resolved, err := filepath.Abs(candidate); err == nil {
				if _, err := os.Stat(resolved); err == nil {
					return resolved, nil
				}
			}
		}
	}
	// Check next to this binary
	if exe, err := os.Executable(); err == nil {
		for _, candidate := range []string{
			filepath.Join(filepath.Dir(exe), "pilot"),
			filepath.Join(filepath.Dir(exe), "..", "pilot"),
		} {
			if resolved, err := filepath.Abs(candidate); err == nil {
				if _, err := os.Stat(resolved); err == nil {
					return resolved, nil
				}
			}
		}
	}
	return "", fmt.Errorf("pilot binary not found")
}

// StartServe launches `pilot serve` in the background.
// Always stops any existing serve first to pick up binary changes.
func StartServe() error {
	StopServe()
	KillLingering()

	bin, err := FindPilotBinary()
	if err != nil {
		return err
	}

	logPath := filepath.Join(pilotDir(), "pilot-serve.log")
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
	if err != nil {
		return fmt.Errorf("failed to open pilot serve log: %w", err)
	}

	cmd := exec.Command(bin, "serve")
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	if err := cmd.Start(); err != nil {
		logFile.Close()
		return fmt.Errorf("failed to start pilot serve: %w", err)
	}

	pidPath := filepath.Join(pilotDir(), "pilot-serve.pid")
	os.WriteFile(pidPath, []byte(fmt.Sprintf("%d", cmd.Process.Pid)), 0644)
	go func() {
		_ = cmd.Wait()
		_ = logFile.Close()
	}()

	port := readSSEPort()
	if err := waitForServe(port, 3*time.Second); err != nil {
		logData, _ := os.ReadFile(logPath)
		return fmt.Errorf("pilot serve failed to start: %w\n\nServer log:\n%s", err, string(logData))
	}

	return nil
}

// StopServe stops the background `pilot serve` process.
// Kills by PID file first, then by port as a fallback.
func StopServe() error {
	pidPath := filepath.Join(pilotDir(), "pilot-serve.pid")
	_ = stopPid(pidPath)
	// Fallback: kill anything on our port in case pid file was stale
	killPort(readSSEPort())
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

func killPort(port int) {
	out, err := exec.Command("lsof", fmt.Sprintf("-ti:%d", port)).Output()
	if err != nil || len(out) == 0 {
		return
	}
	for _, pidStr := range strings.Fields(strings.TrimSpace(string(out))) {
		if pid, err := strconv.Atoi(pidStr); err == nil {
			syscall.Kill(pid, syscall.SIGTERM)
		}
	}
	// Wait briefly then force kill
	time.Sleep(500 * time.Millisecond)
	out, err = exec.Command("lsof", fmt.Sprintf("-ti:%d", port)).Output()
	if err != nil || len(out) == 0 {
		return
	}
	for _, pidStr := range strings.Fields(strings.TrimSpace(string(out))) {
		if pid, err := strconv.Atoi(pidStr); err == nil {
			syscall.Kill(pid, syscall.SIGKILL)
		}
	}
}

// IsServeRunning checks if the serve process is alive.
func IsServeRunning() bool {
	pidPath := filepath.Join(pilotDir(), "pilot-serve.pid")
	return isPidAlive(pidPath)
}

// KillLingering kills any running pilot hook processes.
func KillLingering() error {
	exec.Command("pkill", "-f", "pilot approve").Run()
	exec.Command("pkill", "-f", "pilot interrogate").Run()
	exec.Command("pkill", "-f", "pilot on-stop").Run()
	exec.Command("pkill", "-f", "pilot codex-approve").Run()
	exec.Command("pkill", "-f", "pilot codex-interrogate").Run()
	exec.Command("pkill", "-f", "pilot codex-on-stop").Run()
	return nil
}

func isPidAlive(pidPath string) bool {
	data, err := os.ReadFile(pidPath)
	if err != nil {
		return false
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		return false
	}
	return syscall.Kill(pid, 0) == nil
}

func stopPid(pidPath string) error {
	data, err := os.ReadFile(pidPath)
	if err != nil {
		return nil
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		os.Remove(pidPath)
		return nil
	}
	syscall.Kill(pid, syscall.SIGTERM)
	for i := 0; i < 20; i++ {
		time.Sleep(100 * time.Millisecond)
		if syscall.Kill(pid, 0) != nil {
			os.Remove(pidPath)
			return nil
		}
	}
	syscall.Kill(pid, syscall.SIGKILL)
	os.Remove(pidPath)
	return nil
}
