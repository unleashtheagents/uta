package cli

import (
	"errors"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/unleashtheagents/uta/internal/profile"
)

// newVibeCmd implements `uta vibe <mode> "<directive>"` — a per-mode
// steering knob the user can nudge between runs without editing the
// profile YAML. Each invocation appends a timestamped entry to
// <project>/.uta/profiles/<mode>.vibe.md; the file is consumed by the
// audit pipeline, which appends it to every persona prompt as a
// `## Current vibe` section.
func newVibeCmd() *cobra.Command {
	var (
		clear bool
		show  bool
	)
	cmd := &cobra.Command{
		Use:   "vibe <mode> [\"<directive>\"]",
		Short: "nudge a mode's behavior between runs (per-project steering)",
		Long: `Vibes are short, free-form directives appended to every persona prompt
when a mode runs. They live at <project>/.uta/profiles/<mode>.vibe.md
and are accumulated as timestamped entries so the history is preserved.

Examples:
  uta vibe audit "be more aggressive — flag anything that smells off"
  uta vibe audit --show
  uta vibe audit --clear`,
		Args: cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			modeName := strings.TrimSpace(args[0])
			if modeName == "" {
				return errors.New("vibe: mode name is required")
			}
			if show && clear {
				return errors.New("vibe: --show and --clear are mutually exclusive")
			}

			app, err := newApp(cmd.Context())
			if err != nil {
				return err
			}
			defer app.Close()

			if err := requireProject(app); err != nil {
				return err
			}

			// Typo guard: a vibe for a mode that doesn't exist is silently
			// useless — nothing ever loads it. Surface the mismatch on
			// stderr so the user can correct the spelling, but don't abort:
			// the user may legitimately be staging a vibe ahead of writing
			// the profile YAML.
			warnIfUnknownMode(cmd, app, modeName)

			switch {
			case show:
				if len(args) > 1 {
					return errors.New("vibe: --show takes no directive argument")
				}
				body, rerr := profile.ReadVibe(app.ProjectRoot, modeName)
				if rerr != nil {
					return rerr
				}
				if strings.TrimSpace(body) == "" {
					fmt.Fprintf(cmd.ErrOrStderr(), "no vibe set for mode %q\n", modeName)
					return nil
				}
				fmt.Fprint(cmd.OutOrStdout(), body)
				return nil

			case clear:
				if len(args) > 1 {
					return errors.New("vibe: --clear takes no directive argument")
				}
				if err := profile.ClearVibe(app.ProjectRoot, modeName); err != nil {
					return err
				}
				fmt.Fprintf(cmd.ErrOrStderr(), "cleared vibe for mode %q\n", modeName)
				return nil
			}

			if len(args) < 2 {
				return errors.New("vibe: directive is required (or pass --show / --clear)")
			}
			directive := strings.TrimSpace(args[1])
			if directive == "" {
				return errors.New("vibe: directive must not be empty")
			}
			if err := profile.AppendVibe(app.ProjectRoot, modeName, directive); err != nil {
				return err
			}
			fmt.Fprintf(cmd.ErrOrStderr(),
				"vibe appended to %s\n",
				profile.VibePath(app.ProjectRoot, modeName))
			return nil
		},
	}
	cmd.Flags().BoolVar(&clear, "clear", false, "remove the mode's vibe file")
	cmd.Flags().BoolVar(&show, "show", false, "print the mode's current vibe")
	return cmd
}

// warnIfUnknownMode emits a one-line stderr warning when modeName does not
// match any profile resolvable from the user's global + project dirs. It
// never returns an error — the warning is informational and the calling
// command continues unchanged.
func warnIfUnknownMode(cmd *cobra.Command, app *App, modeName string) {
	profiles, _ := profile.LoadAll(app.GlobalHome, app.ProjectRoot)
	for _, p := range profiles {
		if p.Name == modeName {
			return
		}
	}
	fmt.Fprintf(cmd.ErrOrStderr(),
		"warn: no profile named %q — this vibe will not be loaded until you create the profile (uta mode list to see what exists)\n",
		modeName)
}
