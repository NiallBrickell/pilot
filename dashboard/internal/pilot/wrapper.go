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

func pidFilePath() string {
	return filepath.Join(pilotDir(), "pilot.pid")
}

func servePidFilePath() string {
	return filepath.Join(pilotDir(), "pilot-serve.pid")
}

func IsWrapperRunning() bool {
	return isPidAlive(pidFilePath())
}

func IsServeRunning() bool {
	return isPidAlive(servePidFilePath())
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

func StartServe() error {
	if IsServeRunning() {
		return nil
	}

	bin, err := findPilotBinary()
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

	if err := os.WriteFile(servePidFilePath(), []byte(fmt.Sprintf("%d", cmd.Process.Pid)), 0644); err != nil {
		return fmt.Errorf("failed to write serve pidfile: %w", err)
	}

	go cmd.Wait()

	return nil
}

func StopServe() error {
	return stopPid(servePidFilePath())
}

func StartWrapper() error {
	if IsWrapperRunning() {
		return fmt.Errorf("pilot wrapper is already running")
	}

	bin, err := findPilotBinary()
	if err != nil {
		return err
	}

	script := fmt.Sprintf(`tell application "Terminal"
	activate
	do script "cd %s && %s wrap"
end tell`, filepath.Dir(bin), bin)

	cmd := exec.Command("osascript", "-e", script)
	return cmd.Run()
}

func StopWrapper() error {
	return stopPid(pidFilePath())
}

func KillLingering() error {
	cmd := exec.Command("pkill", "-f", "pilot approve")
	_ = cmd.Run()
	cmd = exec.Command("pkill", "-f", "pilot on-stop")
	_ = cmd.Run()
	return nil
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

	if err := syscall.Kill(pid, syscall.SIGTERM); err != nil {
		os.Remove(pidPath)
		return nil
	}

	for i := 0; i < 20; i++ {
		time.Sleep(100 * time.Millisecond)
		if err := syscall.Kill(pid, 0); err != nil {
			os.Remove(pidPath)
			return nil
		}
	}

	_ = syscall.Kill(pid, syscall.SIGKILL)
	os.Remove(pidPath)
	return nil
}
