package pty

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/NiallBrickell/pilot/internal/config"
	"github.com/NiallBrickell/pilot/internal/server"
	"github.com/NiallBrickell/pilot/internal/state"

	"github.com/creack/pty"
	"golang.org/x/term"
)

// Run wraps a Claude Code session in a monitored PTY.
func Run(claudeArgs []string) error {
	cfg := config.Load()

	// Write PID file
	pidPath := config.PidFilePath()
	if err := os.WriteFile(pidPath, []byte(fmt.Sprintf("%d", os.Getpid())), 0644); err != nil {
		slog.Warn("Could not write pid file", "error", err)
	}
	defer os.Remove(pidPath)

	// Start SSE server
	srv := server.New(cfg.General.SSEPort)
	go func() {
		if err := srv.Start(); err != nil {
			slog.Warn("SSE server error", "error", err)
		}
	}()
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		srv.Shutdown(ctx)
	}()

	// Publish session start event
	publishStateChange(srv.Broker(), true)

	// Record session start
	pilotState, _ := state.ReadState()
	pilotState.SessionActive = true
	now := time.Now().UTC()
	pilotState.SessionStart = &now
	pilotState.PendingResponse = nil
	_ = state.WriteState(&pilotState)

	// Build the claude command
	cmd := exec.Command("claude", claudeArgs...)
	cmd.Env = os.Environ()

	// Start in PTY
	ptmx, err := pty.Start(cmd)
	if err != nil {
		return err
	}
	defer ptmx.Close()

	// Handle terminal resize
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGWINCH)
	go func() {
		for range ch {
			if err := pty.InheritSize(os.Stdin, ptmx); err != nil {
				slog.Debug("Error resizing pty", "error", err)
			}
		}
	}()
	// Initial resize
	ch <- syscall.SIGWINCH

	// Set stdin to raw mode
	oldState, err := term.MakeRaw(int(os.Stdin.Fd()))
	if err != nil {
		slog.Warn("Could not set raw mode", "error", err)
	}
	defer func() {
		if oldState != nil {
			_ = term.Restore(int(os.Stdin.Fd()), oldState)
		}
	}()

	// Channel for injecting responses into PTY
	injectCh := make(chan string, 16)

	// Track when stdout goroutine finishes (Claude exited)
	var stdoutDone sync.WaitGroup
	stdoutDone.Add(1)
	done := make(chan struct{})

	// --- Goroutine 1: PTY → stdout (passthrough) ---
	go func() {
		defer stdoutDone.Done()
		buf := make([]byte, 4096)
		for {
			n, err := ptmx.Read(buf)
			if n > 0 {
				os.Stdout.Write(buf[:n])
			}
			if err != nil {
				if err != io.EOF {
					slog.Debug("PTY read error (likely exit)", "error", err)
				}
				return
			}
		}
	}()

	// --- Goroutine 2: stdin → PTY (passthrough) ---
	go func() {
		buf := make([]byte, 1024)
		for {
			n, err := os.Stdin.Read(buf)
			if n > 0 {
				ptmx.Write(buf[:n])
			}
			if err != nil {
				return
			}
		}
	}()

	// --- Goroutine 3: Inject auto-responses into PTY ---
	go func() {
		for msg := range injectCh {
			ptmx.Write([]byte(msg + "\n"))
			slog.Info("Injected auto-response", "message", msg)
		}
	}()

	// Wait for stdout goroutine to finish in a separate goroutine
	go func() {
		stdoutDone.Wait()
		close(done)
	}()

	// Handle SIGINT/SIGTERM for cleanup
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	// --- Main loop: Poll state.json for pending responses ---
	pollInterval := time.Duration(cfg.General.IdleTimeoutMs) * time.Millisecond
	if pollInterval < time.Second {
		pollInterval = time.Second
	}
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	running := true
	for running {
		select {
		case <-done:
			slog.Info("Claude process exited")
			running = false
		case <-sigCh:
			slog.Info("Received signal, shutting down")
			running = false
		case <-ticker.C:
			if ps, err := state.ReadState(); err == nil {
				if ps.PendingResponse != nil {
					age := time.Since(ps.PendingResponse.Timestamp).Seconds()
					if int64(age) < cfg.General.PendingResponseMaxAge {
						slog.Info("Injecting pending response",
							"confidence", ps.PendingResponse.Confidence,
							"age_s", int(age),
							"message", ps.PendingResponse.Message,
						)
						injectCh <- ps.PendingResponse.Message
					} else {
						slog.Debug("Discarding stale pending response", "age_s", int(age))
					}

					ps.PendingResponse = nil
					_ = state.WriteState(&ps)
				}
			}
		}
	}

	// Cleanup
	close(injectCh)
	signal.Stop(ch)
	signal.Stop(sigCh)

	pilotState, _ = state.ReadState()
	pilotState.SessionActive = false
	pilotState.PendingResponse = nil
	_ = state.WriteState(&pilotState)

	// Publish session end event
	publishStateChange(srv.Broker(), false)

	slog.Info("Pilot session ended")
	return nil
}

func publishStateChange(broker *server.Broker, active bool) {
	data, _ := json.Marshal(map[string]any{
		"session_active": active,
		"timestamp":      time.Now().UTC().Format(time.RFC3339Nano),
	})
	broker.Publish(server.SSEEvent{
		Type: "state_change",
		Data: string(data),
	})
}
