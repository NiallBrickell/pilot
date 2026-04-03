package cmd

import (
	"fmt"
	"strings"

	"github.com/NiallBrickell/pilot/internal/state"

	"github.com/spf13/cobra"
)

func init() {
	rootCmd.AddCommand(&cobra.Command{
		Use:   "profile",
		Short: "Show evaluation timing stats (avg, p50, p95, p99 by source)",
		RunE:  runProfile,
	})
}

func runProfile(cmd *cobra.Command, args []string) error {
	overall := state.ReadProfileAll(0)
	if overall == nil || overall.Count == 0 {
		fmt.Println("No evaluation timing data yet. Run some tool calls with pilot enabled first.")
		return nil
	}

	bySource := state.ReadProfile(0)

	fmt.Println("Evaluation Timing Profile")
	fmt.Println(strings.Repeat("─", 80))
	fmt.Printf("%-20s %6s %8s %8s %8s %8s %8s %8s\n", "SOURCE", "COUNT", "AVG", "P50", "P95", "P99", "MIN", "MAX")
	fmt.Println(strings.Repeat("─", 80))

	for _, s := range bySource {
		fmt.Printf("%-20s %6d %7.1fms %7.1fms %7.1fms %7.1fms %7.1fms %7.1fms\n",
			s.Source, s.Count, s.AvgMs, s.P50Ms, s.P95Ms, s.P99Ms, s.MinMs, s.MaxMs)
	}

	fmt.Println(strings.Repeat("─", 80))
	fmt.Printf("%-20s %6d %7.1fms %7.1fms %7.1fms %7.1fms %7.1fms %7.1fms\n",
		"TOTAL", overall.Count, overall.AvgMs, overall.P50Ms, overall.P95Ms, overall.P99Ms, overall.MinMs, overall.MaxMs)
	fmt.Println()

	return nil
}
