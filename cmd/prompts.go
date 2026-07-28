package cmd

import (
	"fmt"

	"github.com/erdoai/pilot/internal/config"
	"github.com/erdoai/pilot/internal/paths"
	"github.com/spf13/cobra"
)

func init() {
	promptsCmd := &cobra.Command{
		Use:   "prompts",
		Short: "Inspect and sync the evaluator prompts in pilot.toml",
	}

	promptsCmd.AddCommand(&cobra.Command{
		Use:   "status",
		Short: "Show whether pilot.toml has the current default prompts",
		RunE:  runPromptsStatus,
	})

	promptsCmd.AddCommand(&cobra.Command{
		Use:   "reset",
		Short: "Replace the [prompts] section with this build's defaults",
		Long: "Replace the [prompts] section of pilot.toml with the defaults compiled into this\n" +
			"binary, backing up the old file first. Everything outside [prompts] is left alone.\n" +
			"Use this when `status` reports customised prompts you didn't mean to keep — that\n" +
			"state stops new default prompts from ever reaching the evaluator.",
		RunE: runPromptsReset,
	})

	rootCmd.AddCommand(promptsCmd)
}

func runPromptsStatus(cmd *cobra.Command, args []string) error {
	status, err := paths.PromptsStatusOf(config.EmbeddedConfig(), config.ShippedPromptHashes())
	if err != nil {
		return err
	}

	fmt.Printf("config:   %s\n", paths.ConfigFile())
	fmt.Printf("state:    %s\n", status.State)
	fmt.Printf("yours:    %s\n", short(status.UserHash))
	fmt.Printf("default:  %s\n", short(status.EmbeddedHash))
	fmt.Printf("baseline: %s\n", short(status.BaselineHash))

	switch status.State {
	case paths.PromptsUpToDate:
		fmt.Println("\nYou have this build's default prompts.")
	case paths.PromptsBehind:
		fmt.Println("\nYou're on an older default. `pilot serve` will upgrade you on next start.")
	case paths.PromptsCustomised:
		fmt.Println("\nYour prompts differ from every default pilot has shipped, so they're")
		fmt.Println("treated as yours and left alone — new defaults will NOT reach the evaluator.")
		fmt.Println("Run `pilot prompts reset` to take this build's defaults instead.")
	case paths.PromptsBootstrapped:
		fmt.Println("\nRecorded your current prompts as the baseline. Future default changes will apply.")
	}
	return nil
}

func runPromptsReset(cmd *cobra.Command, args []string) error {
	result, err := paths.ResetPromptsToDefault(config.EmbeddedConfig())
	if err != nil {
		return err
	}
	if !result.Upgraded {
		fmt.Printf("Nothing to do (%s).\n", result.Reason)
		return nil
	}
	fmt.Printf("Reset prompts to this build's defaults.\nBacked up your old config to %s\n", result.BackupPath)
	fmt.Println("Restart pilot for the change to take effect: pilot start")
	return nil
}

func short(hash string) string {
	if hash == "" {
		return "(none)"
	}
	if len(hash) > 12 {
		return hash[:12]
	}
	return hash
}
