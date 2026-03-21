package cmd

import (
	"encoding/json"
	"fmt"

	"github.com/NiallBrickell/pilot/internal/state"

	"github.com/spf13/cobra"
)

func init() {
	rootCmd.AddCommand(&cobra.Command{
		Use:   "status",
		Short: "Show current pilot state (for dashboard integration)",
		RunE:  runStatus,
	})
}

func runStatus(cmd *cobra.Command, args []string) error {
	s, err := state.ReadState()
	if err != nil {
		return err
	}
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	fmt.Println(string(data))
	return nil
}
