// Shell-completion helpers. Each ValidArgsFunction opens a short-lived App
// to query real state (sessions, profiles, threads) so `uta resume <TAB>`
// offers the ids the user actually has. Failures degrade to "no
// completions" — completion must never error at the prompt.
package cli

import (
	"github.com/spf13/cobra"

	"github.com/unleashtheagents/uta/internal/profile"
	"github.com/unleashtheagents/uta/internal/state"
)

// completeSessionIDs offers the short ids of recent sessions, annotated
// with status and goal so the shell's menu is self-describing.
func completeSessionIDs(cmd *cobra.Command, args []string, _ string) ([]cobra.Completion, cobra.ShellCompDirective) {
	if len(args) > 0 {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	app, err := newApp(cmd.Context())
	if err != nil {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	defer app.Close()
	rows, err := app.Store.ListSessions(25, 0, "")
	if err != nil {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	out := make([]cobra.Completion, 0, len(rows))
	for _, s := range rows {
		out = append(out, cobra.CompletionWithDesc(shortID(s.ID), s.Status+" — "+truncateLine(s.Goal, 48)))
	}
	return out, cobra.ShellCompDirectiveNoFileComp
}

// completeModeNames offers the names of loadable MissionProfiles.
func completeModeNames(cmd *cobra.Command, args []string, _ string) ([]cobra.Completion, cobra.ShellCompDirective) {
	if len(args) > 0 {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	app, err := newApp(cmd.Context())
	if err != nil {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	defer app.Close()
	profiles, _ := profile.LoadAll(app.GlobalHome, app.ProjectRoot)
	out := make([]cobra.Completion, 0, len(profiles))
	for _, p := range profiles {
		out = append(out, cobra.CompletionWithDesc(p.Name, p.Description))
	}
	return out, cobra.ShellCompDirectiveNoFileComp
}

// completeThreadNames offers the names of existing threads.
func completeThreadNames(cmd *cobra.Command, args []string, _ string) ([]cobra.Completion, cobra.ShellCompDirective) {
	if len(args) > 0 {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	app, err := newApp(cmd.Context())
	if err != nil {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	defer app.Close()
	idx, err := state.Load(app.StateDir)
	if err != nil {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	out := make([]cobra.Completion, 0, len(idx.Threads))
	for _, t := range idx.Threads {
		out = append(out, cobra.Completion(t.Name))
	}
	return out, cobra.ShellCompDirectiveNoFileComp
}
