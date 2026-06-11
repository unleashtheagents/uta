package cli

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/spf13/cobra"

	profileperf "github.com/unleashtheagents/uta/internal/profile_perf"
	"github.com/unleashtheagents/uta/internal/store"
)

func newPerfCmd() *cobra.Command {
	var (
		modeName string
		sinceStr string
		format   string
		limit    int
		showCost bool
	)
	cmd := &cobra.Command{
		Use:   "perf [session-id]",
		Short: "post-hoc latency + cost breakdown of a run (or aggregate across recent runs)",
		Long: `Read recorded trajectory events and report where time went in a run:
planner, per-subtask provider calls, synthesis, gates, external tools,
and an FIFO-paired provider-tool breakdown (Read, Bash, Edit, ...).

  uta perf <session-id>                  single-session ascii flame summary
  uta perf --mode dev --since 7d         aggregate latency across sessions in mode 'dev'
  uta perf --cost --since 30d            cost rollup by mode/provider/day (last 30 days)

Wall-clock is from the first to the last persisted event; cpu-equivalent
is the non-parallel sum of measured phases, so parallel subtasks push the
ratio above 1.0.

--since accepts standard Go durations ("168h", "30m") and a 'd' suffix
("7d", "30d") that other Go-CLI tools commonly support. Both work.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			app, err := newApp(cmd.Context())
			if err != nil {
				return err
			}
			defer app.Close()

			since, err := parseSinceDuration(sinceStr)
			if err != nil {
				return err
			}

			if len(args) == 1 {
				return runPerfSession(cmd, app, args[0], format)
			}
			if since <= 0 {
				since = 7 * 24 * time.Hour
			}
			if showCost {
				return runPerfCost(cmd, app, modeName, since, format, limit)
			}
			return runPerfAggregate(cmd, app, modeName, since, format, limit)
		},
	}
	cmd.Flags().StringVar(&modeName, "mode", "", "(aggregate mode) MissionProfile name to filter on")
	cmd.Flags().StringVar(&sinceStr, "since", "7d", "window for aggregate views; accepts Go durations (168h) or 'd' suffix (7d)")
	cmd.Flags().StringVar(&format, "format", "pretty", "output format: pretty|json")
	cmd.Flags().IntVar(&limit, "limit", 200, "(aggregate mode) max sessions to scan")
	cmd.Flags().BoolVar(&showCost, "cost", false, "render the cost rollup (tokens + approx USD) by mode/provider/day instead of latency")
	return cmd
}

func runPerfSession(cmd *cobra.Command, app *App, id, format string) error {
	sess, err := app.Store.GetSession(id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("session not found: %s", id)
		}
		return err
	}
	events, err := app.Store.ListEvents(id, 0, 0)
	if err != nil {
		return err
	}
	b := profileperf.BuildSessionBreakdown(sess, events)
	switch format {
	case "json":
		enc := json.NewEncoder(cmd.OutOrStdout())
		enc.SetIndent("", "  ")
		return enc.Encode(b)
	case "", "pretty":
		profileperf.RenderSession(cmd.OutOrStdout(), b)
		return nil
	default:
		return fmt.Errorf("unknown format: %s", format)
	}
}

func runPerfAggregate(cmd *cobra.Command, app *App, modeName string, since time.Duration, format string, limit int) error {
	cutoff := time.Now().Add(-since)
	candidates, err := pickPerfSessions(app.Store, modeName, cutoff, limit)
	if err != nil {
		return err
	}
	breakdowns := make([]profileperf.Breakdown, 0, len(candidates))
	for _, s := range candidates {
		events, err := app.Store.ListEvents(s.ID, 0, 0)
		if err != nil {
			fmt.Fprintf(cmd.ErrOrStderr(), "warn: skip %s: %v\n", shortID(s.ID), err)
			continue
		}
		breakdowns = append(breakdowns, profileperf.BuildSessionBreakdown(s, events))
	}
	agg := profileperf.BuildAggregate(breakdowns)
	switch format {
	case "json":
		enc := json.NewEncoder(cmd.OutOrStdout())
		enc.SetIndent("", "  ")
		return enc.Encode(agg)
	case "", "pretty":
		profileperf.RenderAggregate(cmd.OutOrStdout(), agg, modeName, cutoff)
		return nil
	default:
		return fmt.Errorf("unknown format: %s", format)
	}
}

// runPerfCost renders a cost rollup grouped by (day, mode, provider).
// Reads CostBucket rows from the store and either pretty-prints an
// aligned ASCII table or emits JSON for pipelines.
func runPerfCost(cmd *cobra.Command, app *App, modeName string, since time.Duration, format string, _ int) error {
	cutoff := time.Now().Add(-since)
	buckets, err := app.Store.CostRollups(cutoff, modeName)
	if err != nil {
		return err
	}
	switch format {
	case "json":
		enc := json.NewEncoder(cmd.OutOrStdout())
		enc.SetIndent("", "  ")
		return enc.Encode(buckets)
	case "", "pretty":
		renderCostBuckets(cmd, buckets, modeName, cutoff)
		return nil
	default:
		return fmt.Errorf("unknown format: %s", format)
	}
}

// renderCostBuckets prints the cost rollup as a fixed-column ASCII
// table. Mirrors the alignment style profileperf uses for latency
// breakdowns so the perf sub-views feel consistent.
func renderCostBuckets(cmd *cobra.Command, buckets []store.CostBucket, modeFilter string, cutoff time.Time) {
	w := cmd.OutOrStdout()
	header := fmt.Sprintf("cost rollup · since %s", cutoff.Format("2006-01-02"))
	if modeFilter != "" {
		header += " · mode=" + modeFilter
	}
	fmt.Fprintln(w, header)
	if len(buckets) == 0 {
		fmt.Fprintln(w, "(no completed subtasks with usage in this window)")
		return
	}
	fmt.Fprintf(w, "%-10s  %-12s  %-10s  %5s  %10s  %10s  %8s\n",
		"day", "mode", "provider", "calls", "tokens_in", "tokens_out", "usd")
	var totalCalls int
	var totalIn, totalOut, totalCents int64
	for _, b := range buckets {
		mode := b.ModeName
		if mode == "" {
			mode = "(none)"
		}
		fmt.Fprintf(w, "%-10s  %-12s  %-10s  %5d  %10d  %10d  %8s\n",
			b.Day, mode, b.Provider, b.Calls, b.TokensIn, b.TokensOut, formatUSDCents(b.USDCents))
		totalCalls += b.Calls
		totalIn += b.TokensIn
		totalOut += b.TokensOut
		totalCents += b.USDCents
	}
	fmt.Fprintf(w, "%-10s  %-12s  %-10s  %5d  %10d  %10d  %8s\n",
		"TOTAL", "", "", totalCalls, totalIn, totalOut, formatUSDCents(totalCents))
}

// formatUSDCents renders integer cents as a dollar amount with 2 decimals.
// Negative-safe (it shouldn't happen, but a sign would still render).
func formatUSDCents(c int64) string {
	sign := ""
	if c < 0 {
		sign = "-"
		c = -c
	}
	return fmt.Sprintf("%s$%d.%02d", sign, c/100, c%100)
}

// pickPerfSessions returns the most recent sessions matching the mode +
// time-window filter. modeName == "" means "any mode (including the
// no-profile bucket)".
func pickPerfSessions(s *store.Store, modeName string, cutoff time.Time, limit int) ([]store.Session, error) {
	if limit <= 0 {
		limit = 200
	}
	all, err := s.ListSessions(limit, 0, "")
	if err != nil {
		return nil, err
	}
	out := all[:0]
	for _, sess := range all {
		if sess.CreatedAt.Before(cutoff) {
			continue
		}
		if modeName != "" && sess.ModeName != modeName {
			continue
		}
		out = append(out, sess)
	}
	return out, nil
}
