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
		lastOnly     bool
		sinceStr     string
	)
	cmd := &cobra.Command{
		Use:   "sessions",
		Short: "list recent orchestration runs",
		Example: `  # most recent runs (newest first)
  uta sessions

  # just the latest session id (for scripting: uta trajectory $(uta sessions --last))
  uta sessions --last

  # failed runs from the past day
  uta sessions --status failed --since 24h`,
		RunE: func(cmd *cobra.Command, args []string) error {
			app, err := newApp(cmd.Context())
			if err != nil {
				return err
			}
			defer app.Close()

			var since time.Duration
			if sinceStr != "" {
				since, err = parseSinceDuration(sinceStr)
				if err != nil {
					return err
				}
			}

			// --last short-circuits everything: print just the most recent
			// matching session id (full id, safe to pipe into any command).
			if lastOnly {
				rows, err := app.Store.ListSessions(1, 0, statusFilter)
				if err != nil {
					return err
				}
				if len(rows) == 0 {
					return fmt.Errorf("no sessions recorded yet")
				}
				fmt.Fprintln(cmd.OutOrStdout(), rows[0].ID)
				return nil
			}

			// A --since window filters client-side; fetch unbounded so the
			// window isn't silently cut short by the row limit, then apply
			// limit/offset to the filtered set.
			fetchLimit, fetchOffset := limit, offset
			if since > 0 {
				fetchLimit, fetchOffset = 0, 0
			}
			rows, err := app.Store.ListSessions(fetchLimit, fetchOffset, statusFilter)
			if err != nil {
				return err
			}
			if since > 0 {
				cutoff := time.Now().Add(-since)
				filtered := rows[:0]
				for _, s := range rows {
					if s.CreatedAt.After(cutoff) {
						filtered = append(filtered, s)
					}
				}
				rows = filtered
				if offset > 0 {
					if offset >= len(rows) {
						rows = rows[len(rows):]
					} else {
						rows = rows[offset:]
					}
				}
				if limit > 0 && len(rows) > limit {
					rows = rows[:limit]
				}
			}

			if asJSON {
				enc := json.NewEncoder(cmd.OutOrStdout())
				enc.SetIndent("", "  ")
				return enc.Encode(rows)
			}

			tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, "ID\tCREATED\tWORKER\tSTATUS\tDURATION\tGOAL")
			for _, s := range rows {
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n",
					shortID(s.ID), s.CreatedAt.Format(time.RFC3339),
					s.Worker, s.Status, sessionDuration(s.CreatedAt, s.CompletedAt),
					truncateLine(s.Goal, 60))
			}
			return tw.Flush()
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit JSON")
	cmd.Flags().StringVar(&statusFilter, "status", "", "filter by status (running|completed|partial|failed|cancelled)")
	cmd.Flags().IntVar(&limit, "limit", 50, "max rows to return (0 for unbounded)")
	cmd.Flags().IntVar(&offset, "offset", 0, "rows to skip before returning results")
	cmd.Flags().BoolVar(&lastOnly, "last", false, "print only the most recent session id (honors --status)")
	cmd.Flags().StringVar(&sinceStr, "since", "", "only sessions created within this window; accepts Go durations (2h) or 'd' suffix (7d)")
	return cmd
}

// sessionDuration renders how long a session ran, or "-" while it has no
// completion timestamp (still running, or crashed before finishing).
func sessionDuration(created time.Time, completed *time.Time) string {
	if completed == nil || completed.IsZero() {
		return "-"
	}
	d := completed.Sub(created).Round(time.Second)
	if d < 0 {
		return "-"
	}
	return d.String()
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
