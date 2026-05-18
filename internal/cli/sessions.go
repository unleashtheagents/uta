package cli

import (
	"encoding/json"
	"fmt"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
)

func newSessionsCmd() *cobra.Command {
	var (
		asJSON       bool
		statusFilter string
		limit        int
		offset       int
	)
	cmd := &cobra.Command{
		Use:   "sessions",
		Short: "list recent orchestration runs",
		RunE: func(cmd *cobra.Command, args []string) error {
			app, err := newApp(cmd.Context())
			if err != nil {
				return err
			}
			defer app.Close()

			rows, err := app.Store.ListSessions(limit, offset, statusFilter)
			if err != nil {
				return err
			}

			if asJSON {
				enc := json.NewEncoder(cmd.OutOrStdout())
				enc.SetIndent("", "  ")
				return enc.Encode(rows)
			}

			tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, "ID\tCREATED\tWORKER\tSTATUS\tGOAL")
			for _, s := range rows {
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n",
					shortID(s.ID), s.CreatedAt.Format(time.RFC3339),
					s.Worker, s.Status, truncateLine(s.Goal, 60))
			}
			return tw.Flush()
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit JSON")
	cmd.Flags().StringVar(&statusFilter, "status", "", "filter by status (running|completed|partial|failed|cancelled)")
	cmd.Flags().IntVar(&limit, "limit", 50, "max rows to return (0 for unbounded)")
	cmd.Flags().IntVar(&offset, "offset", 0, "rows to skip before returning results")
	return cmd
}

func shortID(id string) string {
	if len(id) >= 8 {
		return id[:8]
	}
	return id
}

func truncateLine(s string, max int) string {
	s = singleLine(s)
	if len(s) <= max {
		return s
	}
	return s[:max-1] + "…"
}

func singleLine(s string) string {
	out := make([]byte, 0, len(s))
	for _, r := range s {
		if r == '\n' || r == '\r' {
			r = ' '
		}
		out = append(out, byte(r))
	}
	return string(out)
}

