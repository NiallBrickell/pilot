package cmd

import (
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/NiallBrickell/pilot/internal/config"
	"github.com/NiallBrickell/pilot/internal/paths"
	"github.com/NiallBrickell/pilot/internal/server"

	"github.com/spf13/cobra"
)

func init() {
	rootCmd.AddCommand(&cobra.Command{
		Use:   "serve",
		Short: "Run the SSE event server + evaluator sidecar",
		RunE:  runServe,
	})
}

func runServe(cmd *cobra.Command, args []string) error {
	paths.EnsureDir()
	cfg := config.Load()

	// Find evaluator.mjs
	evaluatorPath := findEvaluator()
	evaluatorDir := filepath.Dir(evaluatorPath)
	stopEvaluator := make(chan struct{})
	go superviseEvaluator(evaluatorPath, evaluatorDir, stopEvaluator)

	srv := server.New(cfg.General.SSEPort)

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		<-sigCh
		slog.Info("Shutting down")
		close(stopEvaluator)
		srv.Shutdown(cmd.Context())
		os.Exit(0)
	}()

	return srv.Start()
}

// findEvaluator locates evaluator.mjs using a multi-step resolution:
// 1. $PILOT_EVALUATOR_PATH env var
// 2. Same directory as the running binary
// 3. ~/.pilot/evaluator.mjs
func findEvaluator() string {
	if p := os.Getenv("PILOT_EVALUATOR_PATH"); p != "" {
		return p
	}
	if exe, err := os.Executable(); err == nil {
		candidate := filepath.Join(filepath.Dir(exe), "evaluator.mjs")
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
	}
	return filepath.Join(paths.PilotDir(), "evaluator.mjs")
}

// superviseEvaluator starts the Node evaluator and restarts it if it crashes.
func superviseEvaluator(evaluatorPath, workDir string, stop chan struct{}) {
	for {
		cmd := exec.Command("node", evaluatorPath)
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		cmd.Dir = workDir

		if err := cmd.Start(); err != nil {
			slog.Warn("Failed to start evaluator", "error", err)
			select {
			case <-stop:
				return
			case <-time.After(5 * time.Second):
				continue
			}
		}

		slog.Info("Evaluator sidecar started", "pid", cmd.Process.Pid)

		// Wait for it to exit
		done := make(chan error, 1)
		go func() { done <- cmd.Wait() }()

		select {
		case <-stop:
			cmd.Process.Signal(syscall.SIGTERM)
			select {
			case <-done:
			case <-time.After(3 * time.Second):
				cmd.Process.Kill()
			}
			return
		case err := <-done:
			slog.Warn("Evaluator crashed, restarting in 2s", "error", err)
			select {
			case <-stop:
				return
			case <-time.After(2 * time.Second):
				// Restart loop
			}
		}
	}
}
