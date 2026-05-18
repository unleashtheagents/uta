package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/unleashtheagents/uta/internal/config"
	"github.com/unleashtheagents/uta/internal/paths"
	"github.com/unleashtheagents/uta/internal/store"
)

func newProjectCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "project",
		Short: "manage uta projects (per-directory workspaces)",
		Long: `A project is a workspace anchored at a directory. It has its own SQLite
database, its own blob store, and a shared context directory that subtasks
in the workflow can read and write. Runs from inside a project tree are
automatically project-scoped — sessions, blobs, and the trajectory live
alongside the code instead of in the global ~/.uta home.

Subcommands:
  init      create .uta/ in the current directory
  info      show details for the project at or above the cwd
  list      list known projects discovered on this machine`,
	}
	cmd.AddCommand(newProjectInitCmd())
	cmd.AddCommand(newProjectInfoCmd())
	cmd.AddCommand(newProjectListCmd())
	return cmd
}

func newProjectInitCmd() *cobra.Command {
	var name string
	var force bool
	cmd := &cobra.Command{
		Use:   "init [name]",
		Short: "create a project workspace in the current directory",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) > 0 {
				name = args[0]
			}
			cwd, err := os.Getwd()
			if err != nil {
				return err
			}
			stateDir := paths.ProjectStateDir(cwd)
			preExisted := false
			if _, err := os.Stat(stateDir); err == nil {
				if !force {
					return fmt.Errorf("%s already exists; pass --force to re-initialize", stateDir)
				}
				preExisted = true
			}
			if err := os.MkdirAll(stateDir, 0o755); err != nil {
				return err
			}
			// If the state dir did not exist before this command, remove it on
			// any subsequent error so the project tree is not left half-built.
			// When --force re-initializes an existing dir we leave it alone:
			// the user's pre-existing state is theirs, not ours to delete.
			success := false
			defer func() {
				if !success && !preExisted {
					os.RemoveAll(stateDir)
				}
			}()

			if _, err := paths.Blobs(stateDir); err != nil {
				return err
			}
			if _, err := paths.ProjectContextDir(cwd); err != nil {
				return err
			}

			proj := &config.Project{Name: name}
			if err := config.SaveProject(cwd, proj); err != nil {
				return err
			}
			// Ensure DB is created and migrated up-front.
			st, err := store.Open(paths.DB(stateDir))
			if err != nil {
				return fmt.Errorf("open project db: %w", err)
			}
			st.Close()
			success = true

			// Record the project in the global index so `uta project list`
			// can find it later. Best-effort; failure is non-fatal.
			if home, herr := paths.Home(); herr == nil {
				appendProjectIndex(home, cwd)
			}

			loaded, _ := config.LoadProject(cwd)
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "initialized project %q at %s\n", loaded.Name, cwd)
			fmt.Fprintf(out, "  state dir:    %s\n", stateDir)
			fmt.Fprintf(out, "  blobs:        %s\n", filepath.Join(stateDir, "blobs"))
			fmt.Fprintf(out, "  context dir:  %s\n", filepath.Join(stateDir, "context"))
			fmt.Fprintln(out)
			fmt.Fprintln(out, "next steps:")
			fmt.Fprintln(out, "  uta doctor                       # verify environment")
			fmt.Fprintln(out, "  uta init --workflow              # scaffold a uta.yaml")
			fmt.Fprintln(out, "  uta run -f uta.yaml -y           # run it (sessions are now project-scoped)")
			return nil
		},
	}
	cmd.Flags().StringVarP(&name, "name", "n", "", "human-readable project name (default: basename of cwd)")
	cmd.Flags().BoolVar(&force, "force", false, "re-initialize even if .uta already exists")
	return cmd
}

func newProjectInfoCmd() *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "info",
		Short: "show project details for the current directory",
		RunE: func(cmd *cobra.Command, args []string) error {
			app, err := newApp(cmd.Context())
			if err != nil {
				return err
			}
			defer app.Close()
			if !app.InProject() {
				return errors.New("not inside a project (no .uta/project.yaml found in any ancestor). Run 'uta project init' here to create one.")
			}
			proj, err := config.LoadProject(app.ProjectRoot)
			if err != nil {
				return err
			}
			info := map[string]any{
				"name":         proj.Name,
				"root":         app.ProjectRoot,
				"state_dir":    app.StateDir,
				"context_dir":  app.ContextDir,
				"created_at":   proj.CreatedAt,
				"schema":       proj.SchemaVersion,
				"db":           paths.DB(app.StateDir),
				"global_home":  app.GlobalHome,
			}
			if asJSON {
				enc := json.NewEncoder(cmd.OutOrStdout())
				enc.SetIndent("", "  ")
				return enc.Encode(info)
			}
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "project:     %s\n", proj.Name)
			fmt.Fprintf(out, "root:        %s\n", app.ProjectRoot)
			fmt.Fprintf(out, "state dir:   %s\n", app.StateDir)
			fmt.Fprintf(out, "context:     %s\n", app.ContextDir)
			fmt.Fprintf(out, "created:     %s\n", proj.CreatedAt.Format(time.RFC3339))
			fmt.Fprintf(out, "schema:      %d\n", proj.SchemaVersion)
			fmt.Fprintf(out, "global home: %s\n", app.GlobalHome)
			return nil
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit JSON")
	return cmd
}

func newProjectListCmd() *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "list",
		Short: "list known projects (recorded in the global home)",
		Long: `Reads ~/.uta/projects.index — a newline-delimited list of project roots
recorded the first time each project ran a command. There's no global
registry of projects beyond what uta has seen on this machine.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			home, err := paths.Home()
			if err != nil {
				return err
			}
			idxPath := filepath.Join(home, "projects.index")
			data, err := os.ReadFile(idxPath)
			if err != nil && !os.IsNotExist(err) {
				return err
			}

			type row struct {
				Name string `json:"name"`
				Root string `json:"root"`
				OK   bool   `json:"ok"` // .uta still exists
			}
			var rows []row
			for _, line := range splitLines(string(data)) {
				if line == "" {
					continue
				}
				name := filepath.Base(line)
				if proj, err := config.LoadProject(line); err == nil {
					name = proj.Name
				}
				_, exists := os.Stat(filepath.Join(line, ".uta"))
				rows = append(rows, row{Name: name, Root: line, OK: exists == nil})
			}

			if asJSON {
				enc := json.NewEncoder(cmd.OutOrStdout())
				enc.SetIndent("", "  ")
				return enc.Encode(rows)
			}
			tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, "NAME\tSTATUS\tROOT")
			for _, r := range rows {
				status := "missing"
				if r.OK {
					status = "ok"
				}
				fmt.Fprintf(tw, "%s\t%s\t%s\n", r.Name, status, r.Root)
			}
			if len(rows) == 0 {
				fmt.Fprintln(tw, "(no projects recorded yet)")
			}
			return tw.Flush()
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit JSON")
	return cmd
}

// appendProjectIndex appends root (deduped) to ~/.uta/projects.index.
func appendProjectIndex(globalHome, root string) {
	idx := filepath.Join(globalHome, "projects.index")
	existing, _ := os.ReadFile(idx)
	for _, line := range splitLines(string(existing)) {
		if line == root {
			return // already recorded
		}
	}
	f, err := os.OpenFile(idx, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.WriteString(root + "\n")
}

func splitLines(s string) []string {
	var out []string
	start := 0
	for i, c := range s {
		if c == '\n' {
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	if start < len(s) {
		out = append(out, s[start:])
	}
	return out
}
