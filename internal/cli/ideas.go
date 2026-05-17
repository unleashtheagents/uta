package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/unleashtheagents/uta/internal/improve"
)

func newIdeasCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "ideas",
		Short: "manage the self-improvement idea backlog (per-project)",
		Long: `An idea is a small, actionable code change. Ideas live in the project's
SQLite database (so they persist across runs) with a status lifecycle:

  proposed   gathered or manually added; waiting to be picked up
  accepted   manually promoted (signals 'do this next' to the loop)
  in_progress the improve loop is currently executing it
  done       executed successfully, verification gate passed
  failed     execution or gate failed; LastError carries the reason
  rejected   manually dismissed

uta works on this backlog via `+"`uta improve`"+`. Subcommands here are for
inspection + manual curation.`,
	}
	cmd.AddCommand(
		newIdeasListCmd(),
		newIdeasShowCmd(),
		newIdeasAddCmd(),
		newIdeasGatherCmd(),
		newIdeasAcceptCmd(),
		newIdeasRejectCmd(),
		newIdeasDoneCmd(),
		newIdeasRemoveCmd(),
	)
	return cmd
}

func newIdeasListCmd() *cobra.Command {
	var (
		asJSON   bool
		status   string
		severity string
		limit    int
		newest   bool
	)
	cmd := &cobra.Command{
		Use:   "list",
		Short: "list ideas on the board",
		RunE: func(cmd *cobra.Command, args []string) error {
			app, err := newApp(cmd.Context())
			if err != nil {
				return err
			}
			defer app.Close()
			board := improve.NewBoard(app.Store)
			ideas, err := board.List(improve.ListOptions{
				Status:   status,
				Severity: severity,
				Limit:    limit,
				Newest:   newest,
			})
			if err != nil {
				return err
			}
			if asJSON {
				enc := json.NewEncoder(cmd.OutOrStdout())
				enc.SetIndent("", "  ")
				return enc.Encode(ideas)
			}
			stats, _ := board.Stats()
			fmt.Fprintf(cmd.OutOrStdout(), "%d idea(s) · stats:", len(ideas))
			keys := make([]string, 0, len(stats))
			for k := range stats {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys {
				fmt.Fprintf(cmd.OutOrStdout(), " %s=%d", k, stats[k])
			}
			fmt.Fprintln(cmd.OutOrStdout())
			tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, "ID\tSEV\tSTATUS\tSOURCE\tTITLE")
			for _, i := range ideas {
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n",
					shortID(i.ID), i.Severity, i.Status, i.Source, truncateLine(i.Title, 64))
			}
			return tw.Flush()
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit JSON")
	cmd.Flags().StringVar(&status, "status", "", "filter: proposed|accepted|in_progress|done|failed|rejected")
	cmd.Flags().StringVar(&severity, "severity", "", "filter: high|medium|low|info")
	cmd.Flags().IntVar(&limit, "limit", 100, "max rows")
	cmd.Flags().BoolVar(&newest, "newest", false, "sort by created_at desc instead of priority order")
	return cmd
}

func newIdeasShowCmd() *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "show <id>",
		Short: "show one idea in full",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			app, err := newApp(cmd.Context())
			if err != nil {
				return err
			}
			defer app.Close()
			board := improve.NewBoard(app.Store)
			idea, err := board.Get(args[0])
			if err != nil {
				return err
			}
			if asJSON {
				enc := json.NewEncoder(cmd.OutOrStdout())
				enc.SetIndent("", "  ")
				return enc.Encode(idea)
			}
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "id:        %s\n", idea.ID)
			fmt.Fprintf(out, "title:     %s\n", idea.Title)
			fmt.Fprintf(out, "severity:  %s\n", idea.Severity)
			fmt.Fprintf(out, "status:    %s\n", idea.Status)
			fmt.Fprintf(out, "source:    %s\n", idea.Source)
			fmt.Fprintf(out, "created:   %s\n", idea.CreatedAt.Format(time.RFC3339))
			fmt.Fprintf(out, "updated:   %s\n", idea.UpdatedAt.Format(time.RFC3339))
			fmt.Fprintf(out, "attempts:  %d\n", idea.Attempts)
			if idea.LastSession != "" {
				fmt.Fprintf(out, "session:   %s\n", idea.LastSession)
			}
			if len(idea.Tags) > 0 {
				fmt.Fprintf(out, "tags:      %s\n", strings.Join(idea.Tags, ", "))
			}
			fmt.Fprintln(out, "\nbody:")
			fmt.Fprintln(out, idea.Body)
			if idea.LastError != "" {
				fmt.Fprintln(out, "\nlast error:")
				fmt.Fprintln(out, idea.LastError)
			}
			if idea.Summary != "" {
				fmt.Fprintln(out, "\nsummary:")
				fmt.Fprintln(out, idea.Summary)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit JSON")
	return cmd
}

func newIdeasAddCmd() *cobra.Command {
	var title, body, severity string
	var bodyStdin bool
	var tags []string
	cmd := &cobra.Command{
		Use:   "add",
		Short: "manually add an idea to the backlog",
		RunE: func(cmd *cobra.Command, args []string) error {
			if strings.TrimSpace(title) == "" {
				return errors.New("--title is required")
			}
			if bodyStdin {
				data, err := io.ReadAll(cmd.InOrStdin())
				if err != nil {
					return err
				}
				body = string(data)
			}
			app, err := newApp(cmd.Context())
			if err != nil {
				return err
			}
			defer app.Close()
			board := improve.NewBoard(app.Store)
			idea := &improve.Idea{
				Title:    title,
				Body:     body,
				Severity: severity,
				Source:   "manual",
				Tags:     tags,
			}
			if err := board.Insert(idea); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "added %s · %s · %s\n", shortID(idea.ID), idea.Severity, idea.Title)
			return nil
		},
	}
	cmd.Flags().StringVarP(&title, "title", "t", "", "one-line title (required)")
	cmd.Flags().StringVarP(&body, "body", "b", "", "longer description")
	cmd.Flags().BoolVar(&bodyStdin, "body-stdin", false, "read body from stdin instead of --body")
	cmd.Flags().StringVarP(&severity, "severity", "s", "medium", "high|medium|low|info")
	cmd.Flags().StringArrayVar(&tags, "tag", nil, "tag (repeatable)")
	return cmd
}

func newIdeasGatherCmd() *cobra.Command {
	var (
		workerName string
		goalFlag   string
		max        int
		timeout    time.Duration
		tags       []string
	)
	cmd := &cobra.Command{
		Use:   "gather",
		Short: "ask a worker to read the codebase and propose new ideas",
		Long: `Calls the supplied --worker (default: gemini, chosen for its context-window
advantage) with a prompt that says: read the codebase under --workdir, avoid
duplicating ideas already on the backlog, and return up to --max ideas as
JSON. Parses + persists the ideas with status=proposed.

This is the entry point of the self-improvement loop. Run it once to seed
the board, then either call it again periodically or let `+"`uta improve --gather-when-empty`"+`
do it for you automatically.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			app, err := newApp(cmd.Context())
			if err != nil {
				return err
			}
			defer app.Close()

			if _, ok := app.Registry.Get(workerName); !ok {
				return fmt.Errorf("worker %q not registered. Try `uta providers` to see what's available.", workerName)
			}

			board := improve.NewBoard(app.Store)
			existing, _ := board.List(improve.ListOptions{Limit: 200, Newest: true})

			workdir := app.ProjectRoot
			if workdir == "" {
				workdir, _ = os.Getwd()
			}
			env := []string{}
			if app.InProject() {
				env = []string{
					"UTA_PROJECT_ROOT=" + app.ProjectRoot,
					"UTA_CONTEXT_DIR=" + app.ContextDir,
					"UTA_PROJECT_NAME=" + app.ProjectName,
				}
			}

			ctx, cancel := signalContext(cmd.Context())
			defer cancel()

			gatherer := &improve.Gatherer{Registry: app.Registry}
			fmt.Fprintf(cmd.ErrOrStderr(), "gathering ideas via %s (timeout %s, max %d)…\n",
				workerName, timeout, max)
			gres, err := gatherer.Run(ctx, improve.GatherRequest{
				WorkerName: workerName,
				Workdir:    workdir,
				Env:        env,
				Timeout:    timeout,
				MaxIdeas:   max,
				Goal:       goalFlag,
				Existing:   existing,
				Tags:       tags,
			})
			if err != nil {
				if gres != nil && gres.RawResponse != "" {
					fmt.Fprintln(cmd.ErrOrStderr(), "raw response preview:")
					fmt.Fprintln(cmd.ErrOrStderr(), truncateLine(gres.RawResponse, 800))
				}
				return err
			}
			n := 0
			for _, idea := range gres.Ideas {
				if err := board.Insert(idea); err != nil {
					return fmt.Errorf("persist idea: %w", err)
				}
				n++
			}
			fmt.Fprintf(cmd.OutOrStdout(), "gathered %d idea(s) from %s\n", n, workerName)
			return nil
		},
	}
	cmd.Flags().StringVar(&workerName, "worker", "gemini", "provider to call (defaults to gemini for context-window reasons)")
	cmd.Flags().StringVarP(&goalFlag, "goal", "g", "", "optional steering text appended to the gather prompt")
	cmd.Flags().IntVar(&max, "max", 10, "hard cap on ideas returned per call")
	cmd.Flags().DurationVar(&timeout, "timeout", 10*time.Minute, "per-call timeout")
	cmd.Flags().StringArrayVar(&tags, "tag", nil, "tag added to every gathered idea (repeatable)")
	return cmd
}

func newIdeasAcceptCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "accept <id>",
		Short: "promote a proposed idea to status=accepted",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return mutateStatus(cmd, args[0], improve.StatusAccepted, "accepted")
		},
	}
}

func newIdeasRejectCmd() *cobra.Command {
	var note string
	c := &cobra.Command{
		Use:   "reject <id>",
		Short: "mark an idea as rejected (won't be picked by the loop)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			app, err := newApp(cmd.Context())
			if err != nil {
				return err
			}
			defer app.Close()
			board := improve.NewBoard(app.Store)
			opts := improve.SetStatusOpts{}
			if note != "" {
				opts.Summary = &note
			}
			if err := board.SetStatus(args[0], improve.StatusRejected, opts); err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), "rejected", shortID(args[0]))
			return nil
		},
	}
	c.Flags().StringVar(&note, "note", "", "optional rationale stored as the idea's summary")
	return c
}

func newIdeasDoneCmd() *cobra.Command {
	var note string
	c := &cobra.Command{
		Use:   "done <id>",
		Short: "manually mark an idea as done (e.g. you implemented it by hand)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			app, err := newApp(cmd.Context())
			if err != nil {
				return err
			}
			defer app.Close()
			board := improve.NewBoard(app.Store)
			opts := improve.SetStatusOpts{}
			if note != "" {
				opts.Summary = &note
			}
			if err := board.SetStatus(args[0], improve.StatusDone, opts); err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), "done", shortID(args[0]))
			return nil
		},
	}
	c.Flags().StringVar(&note, "note", "", "optional summary of what was done")
	return c
}

func newIdeasRemoveCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "rm <id>",
		Short: "delete an idea permanently",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			app, err := newApp(cmd.Context())
			if err != nil {
				return err
			}
			defer app.Close()
			board := improve.NewBoard(app.Store)
			if err := board.Delete(args[0]); err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), "removed", shortID(args[0]))
			return nil
		},
	}
}

func mutateStatus(cmd *cobra.Command, id string, st improve.Status, verb string) error {
	app, err := newApp(cmd.Context())
	if err != nil {
		return err
	}
	defer app.Close()
	board := improve.NewBoard(app.Store)
	if err := board.SetStatus(id, st, improve.SetStatusOpts{}); err != nil {
		return err
	}
	fmt.Fprintln(cmd.OutOrStdout(), verb, shortID(id))
	return nil
}

var _ = context.Background // keep import alive if future refactor uses it
