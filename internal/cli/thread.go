package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/unleashtheagents/uta/internal/state"
)

func newThreadCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "thread",
		Short: "manage named contexts (threads) that let you juggle multiple workstreams",
		Long: `Threads are named contexts — a "dev-feature-x" thread and an
"ops-monthly-recap" thread can live side-by-side without their session
state colliding. Each thread remembers its mode and the id of the most
recent session that ran while it was active, so 'uta resume' inside a
thread picks up where that thread left off.

State lives in:
  <ProjectRoot>/.uta/state.json   # when run inside a project
  ~/.uta/state.json               # otherwise

Subcommands:
  new      create a new thread
  switch   make an existing thread active
  list     show every thread (with the active one marked)
  current  print the active thread`,
	}
	cmd.AddCommand(newThreadNewCmd())
	cmd.AddCommand(newThreadSwitchCmd())
	cmd.AddCommand(newThreadListCmd())
	cmd.AddCommand(newThreadCurrentCmd())
	return cmd
}

func newThreadNewCmd() *cobra.Command {
	var mode string
	cmd := &cobra.Command{
		Use:   "new <name>",
		Short: "create a new thread and switch to it",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			app, err := newApp(cmd.Context())
			if err != nil {
				return err
			}
			defer app.Close()

			idx, err := state.Load(app.StateDir)
			if err != nil {
				return err
			}
			t, err := idx.New(args[0], mode)
			if err != nil {
				return err
			}
			if _, err := idx.SetActive(t.ID); err != nil {
				return err
			}
			if err := state.Save(app.StateDir, idx); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "created thread %q (id %s, mode %s) — now active\n",
				t.Name, shortID(t.ID), dashIfEmpty(t.Mode))
			fmt.Fprintf(cmd.OutOrStdout(), "state: %s\n", state.Path(app.StateDir))
			return nil
		},
	}
	cmd.Flags().StringVar(&mode, "mode", "", "MissionProfile this thread is for (informational; pass --mode on run/improve to apply it)")
	return cmd
}

func newThreadSwitchCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:               "switch <name|id>",
		Short:             "make an existing thread the active one",
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: completeThreadNames,
		RunE: func(cmd *cobra.Command, args []string) error {
			app, err := newApp(cmd.Context())
			if err != nil {
				return err
			}
			defer app.Close()

			idx, err := state.Load(app.StateDir)
			if err != nil {
				return err
			}
			t, err := idx.SetActive(args[0])
			if err != nil {
				return err
			}
			if err := state.Save(app.StateDir, idx); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "switched to %q (id %s, mode %s)\n",
				t.Name, shortID(t.ID), dashIfEmpty(t.Mode))
			if t.ActiveSessionID != "" {
				fmt.Fprintf(cmd.OutOrStdout(), "last session: %s\n", t.ActiveSessionID)
			}
			return nil
		},
	}
	return cmd
}

func newThreadListCmd() *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "list",
		Short: "list every thread (active is marked with *)",
		RunE: func(cmd *cobra.Command, args []string) error {
			app, err := newApp(cmd.Context())
			if err != nil {
				return err
			}
			defer app.Close()

			idx, err := state.Load(app.StateDir)
			if err != nil {
				return err
			}
			if asJSON {
				enc := json.NewEncoder(cmd.OutOrStdout())
				enc.SetIndent("", "  ")
				return enc.Encode(idx)
			}
			if len(idx.Threads) == 0 {
				fmt.Fprintln(cmd.OutOrStdout(), "(no threads yet — create one with 'uta thread new <name>')")
				return nil
			}
			tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, "ACTIVE\tNAME\tMODE\tLAST USED\tLAST SESSION\tID")
			for _, t := range idx.Threads {
				marker := " "
				if t.ID == idx.ActiveThread {
					marker = "*"
				}
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n",
					marker, t.Name, dashIfEmpty(t.Mode),
					t.LastUsedAt.Format(time.RFC3339),
					dashIfEmpty(t.ActiveSessionID),
					shortID(t.ID))
			}
			return tw.Flush()
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit JSON instead of a table")
	return cmd
}

func newThreadCurrentCmd() *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "current",
		Short: "print the active thread",
		RunE: func(cmd *cobra.Command, args []string) error {
			app, err := newApp(cmd.Context())
			if err != nil {
				return err
			}
			defer app.Close()

			idx, err := state.Load(app.StateDir)
			if err != nil {
				return err
			}
			t := idx.Active()
			if t == nil {
				return errors.New("no active thread (create one with 'uta thread new <name>')")
			}
			if asJSON {
				enc := json.NewEncoder(cmd.OutOrStdout())
				enc.SetIndent("", "  ")
				return enc.Encode(t)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "name:           %s\n", t.Name)
			fmt.Fprintf(cmd.OutOrStdout(), "id:             %s\n", t.ID)
			fmt.Fprintf(cmd.OutOrStdout(), "mode:           %s\n", dashIfEmpty(t.Mode))
			fmt.Fprintf(cmd.OutOrStdout(), "active session: %s\n", dashIfEmpty(t.ActiveSessionID))
			fmt.Fprintf(cmd.OutOrStdout(), "created:        %s\n", t.CreatedAt.Format(time.RFC3339))
			fmt.Fprintf(cmd.OutOrStdout(), "last used:      %s\n", t.LastUsedAt.Format(time.RFC3339))
			return nil
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit JSON")
	return cmd
}
