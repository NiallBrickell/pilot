package cmd

import (
	"github.com/spf13/cobra"
)

var rootCmd = &cobra.Command{
	Use:   "pilot",
	Short: "AI copilot for Claude Code sessions",
}

func Execute() error {
	return rootCmd.Execute()
}
