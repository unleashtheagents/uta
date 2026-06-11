package cli

import (
	"encoding/json"
	"fmt"
	"sort"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/unleashtheagents/uta/internal/memory"
)

// newMemoryCmd exposes operator-facing views over the institutional-memory
// store. v1 ships `uta memory stats` (count by source_mode) — enough to
// confirm that mode profiles with `memory.consolidate: true` are actually
// producing facts and that lint_rule writes from HIGH audit findings land
// against the expected mode.
func newMemoryCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "memory",
		Short: "inspect the cross-mode institutional-memory store",
	}
	cmd.AddCommand(newMemoryStatsCmd())
	return cmd
}

func newMemoryStatsCmd() *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "stats",
		Short: "show fact counts by source mode",
		RunE: func(cmd *cobra.Command, args []string) error {
			app, err := newApp(cmd.Context())
			if err != nil {
				return err
			}
			defer app.Close()

			store := memory.NewFromEnv(app.Store.DB)
			counts, err := store.CountsByMode()
			if err != nil {
				return err
			}
			total, err := store.Total()
			if err != nil {
				return err
			}

			if asJSON {
				out := map[string]any{
					"total":          total,
					"by_source_mode": counts,
				}
				enc := json.NewEncoder(cmd.OutOrStdout())
				enc.SetIndent("", "  ")
				return enc.Encode(out)
			}

			modes := make([]string, 0, len(counts))
			for m := range counts {
				modes = append(modes, m)
			}
			sort.Strings(modes)

			tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, "SOURCE_MODE\tCOUNT")
			for _, m := range modes {
				label := m
				if label == "" {
					label = "(none)"
				}
				fmt.Fprintf(tw, "%s\t%d\n", label, counts[m])
			}
			fmt.Fprintf(tw, "TOTAL\t%d\n", total)
			return tw.Flush()
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit JSON")
	return cmd
}
