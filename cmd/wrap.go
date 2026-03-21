package cmd

import (
	"github.com/NiallBrickell/pilot/internal/auth"
	ptywrapper "github.com/NiallBrickell/pilot/internal/pty"

	"github.com/spf13/cobra"
)

func init() {
	wrapCmd := &cobra.Command{
		Use:   "wrap [-- claude args...]",
		Short: "Wrap a Claude Code session in a monitored PTY",
		RunE:  runWrap,
		// Accept any args after --
		DisableFlagParsing: true,
	}
	rootCmd.AddCommand(wrapCmd)
}

func runWrap(cmd *cobra.Command, args []string) error {
	// Strip leading "--" if present
	if len(args) > 0 && args[0] == "--" {
		args = args[1:]
	}

	if err := auth.CheckClaudeAuth(); err != nil {
		return err
	}

	return ptywrapper.Run(args)
}
