package cmd

import (
	"github.com/spf13/cobra"
)

// Version is the release version, injected at build time via
// -ldflags "-X github.com/NiallBrickell/pilot/cmd.Version=<tag>". Local builds report "dev".
var Version = "dev"

var rootCmd = &cobra.Command{
	Use:           "pilot",
	Short:         "AI copilot for Claude Code and Codex sessions",
	Version:       Version,
	SilenceUsage:  true,
	SilenceErrors: false,
}

func Execute() error {
	return rootCmd.Execute()
}
