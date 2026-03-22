package pilot

import (
	"fmt"
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
func StartServe() error {
	if IsServeRunning() {
		return nil
	}

	bin, err := FindPilotBinary()
	if err != nil {
		return err
	}

	cmd := exec.Command(bin, "serve")
	cmd.Stdout = nil
	cmd.Stderr = nil
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("failed to start pilot serve: %w", err)
	}

	pidPath := filepath.Join(pilotDir(), "pilot-serve.pid")
	os.WriteFile(pidPath, []byte(fmt.Sprintf("%d", cmd.Process.Pid)), 0644)
	go cmd.Wait()

	return nil
}

// StopServe stops the background `pilot serve` process.
func StopServe() error {
	pidPath := filepath.Join(pilotDir(), "pilot-serve.pid")
	return stopPid(pidPath)
}

// IsServeRunning checks if the serve process is alive.
func IsServeRunning() bool {
	pidPath := filepath.Join(pilotDir(), "pilot-serve.pid")
	return isPidAlive(pidPath)
}

// KillLingering kills any running `pilot approve` or `pilot on-stop` processes.
func KillLingering() error {
	exec.Command("pkill", "-f", "pilot approve").Run()
	exec.Command("pkill", "-f", "pilot on-stop").Run()
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
