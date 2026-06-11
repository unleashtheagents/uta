package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/unleashtheagents/uta/internal/hitl"
)

// newHITLCmd exposes the operator-side of the human-in-the-loop signature
// gate. The async file-drop flow (used when uta is running without a TTY)
// writes a pending.json under <state>/.uta/hitl/<session>/ and blocks until
// a sibling .approved or .denied marker appears. `uta hitl approve` writes
// that marker; `uta hitl list` enumerates what's waiting.
func newHITLCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "hitl",
		Short: "approve, deny, or list pending human-in-the-loop signature requests",
	}
	cmd.AddCommand(newHITLApproveCmd())
	cmd.AddCommand(newHITLListCmd())
	return cmd
}

func newHITLApproveCmd() *cobra.Command {
	var (
		actionID string
		deny     bool
		reason   string
	)
	cmd := &cobra.Command{
		Use:   "approve <session>",
		Short: "approve (or --deny) a pending HITL request on a uta session",
		Long: `Unblock a uta session that is waiting on a human signature in async
mode. uta writes pending requests to:

  <state>/.uta/hitl/<session>/<action-id>.pending.json

This command drops the matching .approved (or, with --deny, .denied) marker
so the supervisor's poll loop can pick it up and proceed (or abort with a
clean hitl_denied trajectory entry).

When --action is omitted, the command picks up the most recent pending
request for the session (stored as pending.json in the same dir).`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			sessionID := strings.TrimSpace(args[0])
			if sessionID == "" {
				return errors.New("session id is required")
			}
			app, err := newApp(cmd.Context())
			if err != nil {
				return err
			}
			defer app.Close()

			if !deny && strings.TrimSpace(reason) != "" {
				return errors.New("--reason is only valid with --deny (approvals do not carry a reason)")
			}

			sessionDir := hitl.SessionDir(app.StateDir, sessionID)
			if actionID == "" {
				id, err := hitl.FirstPending(sessionDir)
				if err != nil {
					return fmt.Errorf("read pending: %w", err)
				}
				if id == "" {
					return fmt.Errorf("no pending HITL request for session %s (looked in %s)", sessionID, sessionDir)
				}
				actionID = id
			}

			if deny {
				if reason == "" {
					reason = "denied via `uta hitl approve --deny`"
				}
				if err := hitl.WriteDenial(sessionDir, actionID, reason); err != nil {
					return fmt.Errorf("write denial: %w", err)
				}
				fmt.Fprintf(cmd.OutOrStdout(), "denied %s/%s: %s\n", sessionID, actionID, reason)
				return nil
			}
			if err := hitl.WriteApproval(sessionDir, actionID); err != nil {
				return fmt.Errorf("write approval: %w", err)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "approved %s/%s\n", sessionID, actionID)
			return nil
		},
	}
	cmd.Flags().StringVar(&actionID, "action", "", "specific action id to approve (defaults to the session's most-recent pending request)")
	cmd.Flags().BoolVar(&deny, "deny", false, "deny instead of approve")
	cmd.Flags().StringVar(&reason, "reason", "", "optional reason recorded into the .denied marker (requires --deny)")
	return cmd
}

func newHITLListCmd() *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "list",
		Short: "list pending HITL signature requests across all sessions",
		Long: `Enumerate every session that has an outstanding HITL request
waiting on a human signature in async (file-drop) mode. Output includes the
session id, action id, action kind, severity, and prompt — enough to decide
whether to run "uta hitl approve" or "uta hitl approve --deny".`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			app, err := newApp(cmd.Context())
			if err != nil {
				return err
			}
			defer app.Close()

			pending, err := hitl.ListPending(app.StateDir)
			if err != nil {
				return fmt.Errorf("list pending: %w", err)
			}

			if asJSON {
				enc := json.NewEncoder(cmd.OutOrStdout())
				enc.SetIndent("", "  ")
				if pending == nil {
					pending = []*hitl.Pending{}
				}
				return enc.Encode(pending)
			}

			if len(pending) == 0 {
				fmt.Fprintln(cmd.OutOrStdout(), "no pending HITL requests")
				return nil
			}
			tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, "SESSION\tACTION_ID\tACTION\tSEVERITY\tREQUESTED\tPROMPT")
			for _, p := range pending {
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n",
					p.SessionID, p.ActionID, dashIfEmpty(p.Action),
					dashIfEmpty(p.Severity), dashIfEmpty(p.RequestedAt),
					truncateLine(p.Prompt, 60))
			}
			return tw.Flush()
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit JSON")
	return cmd
}
