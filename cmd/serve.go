package cmd

import (
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"syscall"

	"github.com/NiallBrickell/pilot/internal/anthropic"
	"github.com/NiallBrickell/pilot/internal/config"
	"github.com/NiallBrickell/pilot/internal/paths"
	"github.com/NiallBrickell/pilot/internal/server"

	"github.com/spf13/cobra"
)

func init() {
	rootCmd.AddCommand(&cobra.Command{
		Use:   "serve",
		Short: "Run the SSE event server",
		RunE:  runServe,
	})
}

func runServe(cmd *cobra.Command, args []string) error {
	paths.EnsureSetup(config.EmbeddedConfig())
	cfg := config.Load()

	// Kill any stale serve process on our port before starting
	port := cfg.General.SSEPort
	if port == 0 {
		port = 9721
	}
	killStalePort(port)

	srv := server.New(cfg)

	// Initialize Anthropic API client for evaluations
	ai, err := anthropic.NewClient(srv.EvalTimeout(), paths.EnvFile())
	if err != nil {
		return fmt.Errorf("anthropic API client: %w", err)
	}
	srv.SetAI(ai)

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		<-sigCh
		slog.Info("Shutting down")
		srv.Shutdown(cmd.Context())
		os.Exit(0)
	}()

	return srv.Start()
}

func killStalePort(port int) {
	out, err := exec.Command("lsof", fmt.Sprintf("-ti:%d", port)).Output()
	if err != nil || len(out) == 0 {
		return
	}
	for _, pidStr := range strings.Fields(strings.TrimSpace(string(out))) {
		pid, err := strconv.Atoi(pidStr)
		if err != nil || pid == os.Getpid() {
			continue
		}
		// Check if it's a pilot process — only kill our own
		cmdOut, err := exec.Command("ps", "-p", pidStr, "-o", "command=").Output()
		if err != nil {
			continue
		}
		cmd := strings.TrimSpace(string(cmdOut))
		if strings.Contains(cmd, "pilot") {
			slog.Info("Killing stale pilot process on port", "port", port, "pid", pid, "cmd", cmd)
			syscall.Kill(pid, syscall.SIGTERM)
		} else {
			slog.Error("Port already in use by non-pilot process", "port", port, "pid", pid, "cmd", cmd)
			fmt.Fprintf(os.Stderr, "Error: port %d is in use by another process (pid %d: %s)\n", port, pid, cmd)
			os.Exit(1)
		}
	}
}
