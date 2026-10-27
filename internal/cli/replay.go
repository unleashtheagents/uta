package cli

import (
	"bufio"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/spf13/cobra"
)

func newReplayCmd() *cobra.Command {
	var (
		delayMs int
		step    bool
		jsonOut bool
	)
	cmd := &cobra.Command{
		Use:   "replay <session-id>",
		Short: "replay a recorded session trajectory step-by-step",
		Long: `Replay the recorded trajectory of a session in real-time or step-by-step.

Flags:
  --delay-ms N   Millisecond delay between events (default 300)
  --step         Wait for Enter key before advancing to next step
  --json         Format events as raw JSON payload during replay`,
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: completeSessionIDs,
		RunE: func(cmd *cobra.Command, args []string) error {
			app, err := newApp(cmd.Context())
			if err != nil {
				return err
			}
			defer app.Close()

			id, err := app.Store.ResolveSessionID(args[0])
			if err != nil {
				return err
			}
			sess, err := app.Store.GetSession(id)
			if err != nil {
				if errors.Is(err, sql.ErrNoRows) {
					return fmt.Errorf("session not found: %s", id)
				}
				return err
			}

			events, err := app.Store.ListEvents(id, 0, 0)
			if err != nil {
				return fmt.Errorf("list events: %w", err)
			}

			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "▶ Replaying session %s (%s) — %d events recorded\n\n", sess.ID[:min(8, len(sess.ID))], sess.Goal, len(events))
			reader := bufio.NewReader(cmd.InOrStdin())

			for i, evt := range events {
				if err := cmd.Context().Err(); err != nil {
					return err
				}
				timestamp := evt.Ts.Format("15:04:05.000")
				subtaskStr := ""
				if evt.SubtaskID != "" {
					subtaskStr = fmt.Sprintf(" [%s]", evt.SubtaskID[:min(8, len(evt.SubtaskID))])
				}

				if jsonOut {
					fmt.Fprintf(out, "[%d/%d] %s%s %s: %s\n", i+1, len(events), timestamp, subtaskStr, evt.Kind, string(evt.Payload))
				} else {
					summary := summarizePayload(evt.Payload)
					fmt.Fprintf(out, "[%d/%d] %s%s %-16s │ %s\n", i+1, len(events), timestamp, subtaskStr, evt.Kind, summary)
				}

				if step {
					fmt.Fprint(out, "   ‹press enter for next step› ")
					_, _ = reader.ReadString('\n')
				} else if delayMs > 0 {
					select {
					case <-cmd.Context().Done():
						return cmd.Context().Err()
					case <-time.After(time.Duration(delayMs) * time.Millisecond):
					}
				}
			}

			fmt.Fprintln(out, "\n✓ Replay complete.")
			return nil
		},
	}
	cmd.Flags().IntVar(&delayMs, "delay-ms", 300, "millisecond delay between events")
	cmd.Flags().BoolVar(&step, "step", false, "wait for enter key between steps")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "print raw json payloads")
	return cmd
}
