package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/unleashtheagents/uta/internal/version"
)

func newHelloCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "hello",
		Short: "60-second guided introduction to uta",
		Long: `Print a guided introduction that explains what uta does, lists which
agent CLIs were detected on PATH, and prints a personalized "first run"
command tailored to the detected providers. Safe to run anywhere — does
not invoke any provider or write outside ~/.uta/.

If you've installed neither claude nor gemini, the tour will tell you
where to get them and what uta does in the meantime.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			app, err := newApp(cmd.Context())
			if err != nil {
				return err
			}
			defer app.Close()
			return runHello(cmd, app)
		},
	}
}

// runHello prints the four-section tour: what uta is, what's installed,
// the one-line "do this next" command, and pointers for deeper reading.
// Pure I/O — no provider calls, no DB writes — so it's safe to run on
// repeat or as part of a smoke test.
func runHello(cmd *cobra.Command, app *App) error {
	out := cmd.OutOrStdout()

	fmt.Fprintln(out, "uta — unleash the agents")
	fmt.Fprintln(out, "  version:", version.String())
	if app.InProject() {
		fmt.Fprintf(out, "  project: %s (%s)\n", app.ProjectName, app.ProjectRoot)
	} else {
		fmt.Fprintln(out, "  scope:   global ~/.uta (no project detected from cwd)")
	}
	fmt.Fprintln(out)

	fmt.Fprintln(out, "What uta does")
	fmt.Fprintln(out, "  Decomposes a goal into parallel subtasks, dispatches each to whichever")
	fmt.Fprintln(out, "  agent CLI you have installed (Claude Code, Gemini CLI, or any YAML-")
	fmt.Fprintln(out, "  described provider), synthesizes the answers, and records every step as")
	fmt.Fprintln(out, "  a queryable trajectory you can resume, audit, or replay.")
	fmt.Fprintln(out)

	dets := app.Registry.DetectAll(cmd.Context())
	avail := availableProviders(app.Registry.Names(), dets)
	fmt.Fprintln(out, "Detected providers")
	if len(avail) == 0 {
		fmt.Fprintln(out, "  (none on PATH)")
		fmt.Fprintln(out, "  Install at least one to run a goal end-to-end:")
		fmt.Fprintln(out, "    - claude    https://docs.claude.com/en/docs/claude-code")
		fmt.Fprintln(out, "    - gemini    https://github.com/google-gemini/gemini-cli")
		fmt.Fprintln(out, "  Or drop a YAML descriptor in ~/.uta/providers/ for any other CLI.")
	} else {
		for _, name := range avail {
			d := dets[name]
			fmt.Fprintf(out, "  - %-10s  %s  (%s)\n", name, d.Version, d.BinaryPath)
		}
	}
	fmt.Fprintln(out)

	fmt.Fprintln(out, "Try this next")
	if len(avail) == 0 {
		fmt.Fprintln(out, "  After installing a provider:")
		fmt.Fprintln(out, "    uta doctor                              # verify the install")
		fmt.Fprintln(out, "    uta hello                               # re-run this tour")
	} else {
		worker := avail[0]
		fmt.Fprintf(out, "  uta run -g \"summarize this directory in 5 bullets\" --worker %s -y\n", worker)
		fmt.Fprintln(out, "  uta sessions                               # list past runs")
		fmt.Fprintln(out, "  uta trajectory <id>                        # full event timeline")
		fmt.Fprintln(out, "  uta resume <id> -g \"now turn each bullet into a tweet\"")
	}
	fmt.Fprintln(out)

	fmt.Fprintln(out, "Or expose uta to another agent")
	fmt.Fprintln(out, "  uta serve --print-mcp-config claude-code > .mcp.json")
	fmt.Fprintln(out, "  uta serve --print-mcp-config cursor      > ~/.cursor/mcp.json")
	fmt.Fprintln(out, "  Full guide:  docs/mcp-server.md")
	fmt.Fprintln(out)

	fmt.Fprintln(out, "Deeper reading")
	fmt.Fprintln(out, "  README.md            quick start + workflow YAML")
	fmt.Fprintln(out, "  examples/profiles/   ready-to-use MissionProfiles (dev/ops/audit/research)")
	fmt.Fprintln(out, "  examples/workflows/  multi-step orchestration recipes")
	fmt.Fprintln(out, "  uta --help           every subcommand, one line each")
	return nil
}
