package cli

import (
	"encoding/json"
	"fmt"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	"github.com/unleashtheagents/uta/internal/profile"
	"github.com/unleashtheagents/uta/internal/store"
)

func newModeCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "mode",
		Short: "list and inspect MissionProfiles",
		Long: `MissionProfiles ("modes") are named bundles of personas, tool allow/deny
lists, environment variables, and policies. They turn uta from
one-shape-fits-all into modal — a 'dev' mode and an 'audit' mode can pull
in different tools, personas, and budgets.

Profiles are discovered as YAML files under:
  ~/.uta/profiles/             # user-wide
  <project>/.uta/profiles/     # project-local (wins on name collision)

A built-in "default" profile is always available so a fresh install has at
least one mode. Override it by writing a file named default.yaml in either
of the dirs above.

Subcommands:
  list   enumerate every available profile
  show   print one profile's full definition`,
	}
	cmd.AddCommand(newModeListCmd())
	cmd.AddCommand(newModeShowCmd())
	return cmd
}

func newModeListCmd() *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "list",
		Short: "list available MissionProfiles",
		RunE: func(cmd *cobra.Command, args []string) error {
			app, err := newApp(cmd.Context())
			if err != nil {
				return err
			}
			defer app.Close()

			profiles, perrs := profile.LoadAll(app.GlobalHome, app.ProjectRoot)
			for _, e := range perrs {
				fmt.Fprintln(cmd.ErrOrStderr(), "warn: profile:", e)
			}

			// Usage stats are joined in by mode name. Missing entries (a
			// profile that has never been used) simply render as zeros — a
			// stat lookup failure is non-fatal so `mode list` works on a
			// fresh repo before the sessions table even has rows.
			stats, statsErr := app.Store.ModeStats(5)
			if statsErr != nil {
				fmt.Fprintln(cmd.ErrOrStderr(), "warn: mode stats:", statsErr)
				stats = map[string]*store.ModeStat{}
			}

			if asJSON {
				type row struct {
					Name                string  `json:"name"`
					Description         string  `json:"description,omitempty"`
					Source              string  `json:"source"`
					Personas            int     `json:"personas"`
					MCPServers          int     `json:"mcp_servers"`
					AllowedTools        int     `json:"allowed_tools"`
					DeniedTools         int     `json:"denied_tools"`
					LastUsedAt          string  `json:"last_used_at,omitempty"`
					TotalSessions       int     `json:"total_sessions"`
					TotalSubtaskSeconds float64 `json:"total_subtask_seconds"`
				}
				out := make([]row, 0, len(profiles))
				for _, p := range profiles {
					r := row{
						Name:         p.Name,
						Description:  firstLine(p.Description),
						Source:       p.Source,
						Personas:     len(p.Personas),
						MCPServers:   len(p.MCPServers),
						AllowedTools: len(p.AllowedTools),
						DeniedTools:  len(p.DeniedTools),
					}
					if ms := stats[p.Name]; ms != nil {
						r.TotalSessions = ms.TotalSessions
						r.TotalSubtaskSeconds = ms.SubtaskSeconds
						if ms.LastUsedAt != nil {
							r.LastUsedAt = ms.LastUsedAt.UTC().Format(time.RFC3339)
						}
					}
					out = append(out, r)
				}
				enc := json.NewEncoder(cmd.OutOrStdout())
				enc.SetIndent("", "  ")
				return enc.Encode(out)
			}

			tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, "NAME\tPERSONAS\tMCP\tALLOW\tDENY\tSESSIONS\tLAST USED\tSOURCE\tDESCRIPTION")
			for _, p := range profiles {
				ms := stats[p.Name]
				lastUsed := "-"
				totalSessions := 0
				if ms != nil {
					totalSessions = ms.TotalSessions
					if ms.LastUsedAt != nil {
						lastUsed = ms.LastUsedAt.UTC().Format(time.RFC3339)
					}
				}
				fmt.Fprintf(tw, "%s\t%d\t%d\t%d\t%d\t%d\t%s\t%s\t%s\n",
					p.Name,
					len(p.Personas),
					len(p.MCPServers),
					len(p.AllowedTools),
					len(p.DeniedTools),
					totalSessions,
					lastUsed,
					sourceLabel(p.Source, app.GlobalHome, app.ProjectRoot),
					dashIfEmpty(firstLine(p.Description)),
				)
			}
			return tw.Flush()
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit JSON instead of a table")
	return cmd
}

func newModeShowCmd() *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "show <name>",
		Short: "print a MissionProfile in full",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			app, err := newApp(cmd.Context())
			if err != nil {
				return err
			}
			defer app.Close()

			profiles, perrs := profile.LoadAll(app.GlobalHome, app.ProjectRoot)
			for _, e := range perrs {
				fmt.Fprintln(cmd.ErrOrStderr(), "warn: profile:", e)
			}
			p, err := profile.Find(profiles, args[0])
			if err != nil {
				return err
			}

			out := cmd.OutOrStdout()
			if asJSON {
				enc := json.NewEncoder(out)
				enc.SetIndent("", "  ")
				return enc.Encode(p)
			}
			fmt.Fprintf(out, "# source: %s\n", p.Source)
			enc := yaml.NewEncoder(out)
			enc.SetIndent(2)
			if err := enc.Encode(p); err != nil {
				return err
			}
			return enc.Close()
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit JSON instead of YAML")
	return cmd
}

// resolveActiveMode looks up the profile named by the root-level --mode flag.
// Returns (nil, nil) when the flag is unset (the no-mode default). On lookup
// failure it prints a clean diagnostic to stderr and returns an exitWith(2)
// error so the run exits with the documented "unknown mode" code.
//
// Per-file load errors from profile.LoadAll are surfaced as warnings on the
// command's stderr but do not abort resolution — a single malformed YAML
// shouldn't make every other profile unreachable.
func resolveActiveMode(cmd *cobra.Command, app *App) (*profile.MissionProfile, error) {
	name, _ := cmd.Flags().GetString("mode")
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, nil
	}
	profiles, perrs := profile.LoadAll(app.GlobalHome, app.ProjectRoot)
	for _, e := range perrs {
		fmt.Fprintln(cmd.ErrOrStderr(), "warn: profile:", e)
	}
	p, err := profile.Find(profiles, name)
	if err != nil {
		fmt.Fprintln(cmd.ErrOrStderr(), "error:", err)
		fmt.Fprintln(cmd.ErrOrStderr(), "hint: run `uta mode list` to see every available mode.")
		return nil, exitWith(2)
	}
	return p, nil
}

// resolveModeByName loads a MissionProfile by explicit name, without
// consulting the --mode flag. Used by `uta eval` where the suite YAML
// names the mode per case. Returns (nil, nil) for empty name.
func resolveModeByName(app *App, name string) (*profile.MissionProfile, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, nil
	}
	profiles, _ := profile.LoadAll(app.GlobalHome, app.ProjectRoot)
	return profile.Find(profiles, name)
}

// sourceLabel renders a profile.Source value compactly for the list table:
// the embedded sentinel stays as-is, and on-disk paths are relativized
// against the project root or global home when possible.
func sourceLabel(src, globalHome, projectRoot string) string {
	if src == "" || src == profile.EmbeddedSource {
		return src
	}
	if projectRoot != "" {
		if rel, ok := stripPrefix(src, projectRoot+"/"); ok {
			return "project:" + rel
		}
	}
	if globalHome != "" {
		if rel, ok := stripPrefix(src, globalHome+"/"); ok {
			return "global:" + rel
		}
	}
	return src
}

func stripPrefix(s, prefix string) (string, bool) {
	if strings.HasPrefix(s, prefix) {
		return s[len(prefix):], true
	}
	return "", false
}

// firstLine returns the first non-empty line of s, useful for table cells
// that can't accommodate multi-line descriptions.
func firstLine(s string) string {
	for _, line := range strings.Split(s, "\n") {
		if t := strings.TrimSpace(line); t != "" {
			return t
		}
	}
	return ""
}
