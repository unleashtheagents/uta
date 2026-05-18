package cli

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/unleashtheagents/uta/internal/store"
)

func newTrajectoryCmd() *cobra.Command {
	var (
		format string
		limit  int
		offset int
	)
	cmd := &cobra.Command{
		Use:   "trajectory <session-id>",
		Short: "render the event timeline of a run",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			app, err := newApp(cmd.Context())
			if err != nil {
				return err
			}
			defer app.Close()

			id := args[0]
			sess, err := app.Store.GetSession(id)
			if err != nil {
				if errors.Is(err, sql.ErrNoRows) {
					return fmt.Errorf("session not found: %s", id)
				}
				return err
			}
			events, err := app.Store.ListEvents(id, limit, offset)
			if err != nil {
				return err
			}

			switch format {
			case "jsonl":
				enc := json.NewEncoder(cmd.OutOrStdout())
				for _, ev := range events {
					_ = enc.Encode(ev)
				}
				return nil
			case "json":
				enc := json.NewEncoder(cmd.OutOrStdout())
				enc.SetIndent("", "  ")
				return enc.Encode(events)
			case "pretty", "":
				return prettyPrintTrajectory(cmd.OutOrStdout(), sess, events)
			default:
				return fmt.Errorf("unknown format: %s", format)
			}
		},
	}
	cmd.Flags().StringVar(&format, "format", "pretty", "output format: pretty|jsonl|json")
	cmd.Flags().IntVar(&limit, "limit", 0, "max events to return (0 for all)")
	cmd.Flags().IntVar(&offset, "offset", 0, "events to skip before returning results")
	return cmd
}

func prettyPrintTrajectory(w interface{ Write([]byte) (int, error) }, sess store.Session, events []store.EventRow) error {
	header := fmt.Sprintf("session %s  worker=%s  status=%s  goal=%s\n\n",
		shortID(sess.ID), sess.Worker, sess.Status, truncateLine(sess.Goal, 80))
	if _, err := w.Write([]byte(header)); err != nil {
		return err
	}
	for _, ev := range events {
		line := fmt.Sprintf("%s  %-22s  %s  %s\n",
			ev.Ts.Format(time.TimeOnly),
			ev.Kind,
			shortID(ifEmpty(ev.SubtaskID, "-")),
			summarizePayload(ev.Payload),
		)
		if _, err := w.Write([]byte(line)); err != nil {
			return err
		}
	}
	return nil
}

func summarizePayload(p json.RawMessage) string {
	if len(p) == 0 {
		return ""
	}
	var m map[string]any
	if err := json.Unmarshal(p, &m); err != nil {
		s := string(p)
		return truncateLine(s, 100)
	}
	// Prefer text-bearing keys.
	for _, k := range []string{"title", "goal", "text", "error", "reason", "status", "spec_id"} {
		if v, ok := m[k].(string); ok && v != "" {
			return truncateLine(k+"="+v, 100)
		}
	}
	for _, k := range []string{"chars", "subtasks", "max_parallel"} {
		if v, ok := m[k]; ok {
			return truncateLine(fmt.Sprintf("%s=%v", k, v), 100)
		}
	}
	return truncateLine(string(p), 100)
}

func ifEmpty(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}
