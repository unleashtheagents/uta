// Package cli wires cobra commands to the engine. Each command is a thin
// shell; all behavior lives in internal/{engine,provider,store,trajectory} so
// a future TUI or MCP-server mode can reuse it without going through cobra.
package cli

import (
	"context"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/unleashtheagents/uta/internal/config"
	"github.com/unleashtheagents/uta/internal/paths"
	"github.com/unleashtheagents/uta/internal/provider"
	"github.com/unleashtheagents/uta/internal/provider/builtin"
	"github.com/unleashtheagents/uta/internal/provider/declarative"
	"github.com/unleashtheagents/uta/internal/store"
	"github.com/unleashtheagents/uta/internal/version"
)

// App is the per-invocation context: where uta is storing state, the open
// DB, the blob store, and the provider registry. Built once per command via
// newApp(). If the cwd is inside a project (a .uta/project.yaml exists
// somewhere up the tree), the App is project-scoped — DB and blobs come from
// the project state dir, not the global home. Providers always come from the
// global home (they're user-wide, not project-specific).
type App struct {
	// GlobalHome is always $UTA_HOME or ~/.uta. Used for providers and as
	// the fallback when no project is active.
	GlobalHome string
	// StateDir is where DB and blobs actually live for this invocation.
	// Equals ProjectRoot/.uta when in a project, otherwise GlobalHome.
	StateDir string
	// ProjectRoot is the directory containing .uta/, or empty if not in a
	// project.
	ProjectRoot string
	// ProjectName is the human-readable name from project.yaml, empty when
	// not in a project.
	ProjectName string
	// ContextDir is ProjectRoot/.uta/context when in a project, empty
	// otherwise. Subtasks see this as $UTA_CONTEXT_DIR.
	ContextDir string

	Store    *store.Store
	Blobs    *store.Blobs
	Registry *provider.Registry
}

func (a *App) Close() {
	if a.Store != nil {
		a.Store.Close()
	}
}

// InProject reports whether this invocation is scoped to a project.
func (a *App) InProject() bool { return a.ProjectRoot != "" }

// newApp resolves paths, opens the store (running migrations if needed), and
// builds a registry seeded with the built-in providers. Auto-detects a
// project by walking up from cwd; falls back to global home when no project
// is found.
func newApp(_ context.Context) (*App, error) {
	globalHome, err := paths.Home()
	if err != nil {
		return nil, fmt.Errorf("resolve global home: %w", err)
	}

	// Project detection: walk up from cwd. We honor $UTA_HOME by skipping
	// project detection entirely when the user has explicitly set it — that
	// preserves the v0.2 ergonomics for ad-hoc runs.
	app := &App{GlobalHome: globalHome, StateDir: globalHome}
	if os.Getenv("UTA_HOME") == "" {
		cwd, _ := os.Getwd()
		if root, ok := paths.FindProjectRoot(cwd); ok {
			if proj, perr := config.LoadProject(root); perr == nil {
				app.ProjectRoot = root
				app.ProjectName = proj.Name
				app.StateDir = paths.ProjectStateDir(root)
				if cdir, cerr := paths.ProjectContextDir(root); cerr == nil {
					app.ContextDir = cdir
				}
			}
		}
	}

	st, err := store.Open(paths.DB(app.StateDir))
	if err != nil {
		return nil, fmt.Errorf("open store: %w", err)
	}
	blobsDir, err := paths.Blobs(app.StateDir)
	if err != nil {
		st.Close()
		return nil, err
	}
	if _, err := paths.ProvidersDir(globalHome); err != nil {
		st.Close()
		return nil, err
	}

	reg := provider.NewRegistry()
	if err := reg.Register(builtin.Claude{}, false); err != nil {
		st.Close()
		return nil, err
	}
	if err := reg.Register(builtin.Gemini{}, false); err != nil {
		st.Close()
		return nil, err
	}

	// Declarative providers from <globalHome>/providers/*.yaml. Errors are
	// non-fatal — we warn but don't abort startup over a malformed descriptor.
	provsDir, _ := paths.ProvidersDir(globalHome)
	descriptors, descErrs := config.LoadProvidersDir(provsDir)
	for _, e := range descErrs {
		fmt.Fprintln(os.Stderr, "warn: provider descriptor:", e)
	}
	for _, d := range descriptors {
		p, err := declarative.New(d)
		if err != nil {
			fmt.Fprintf(os.Stderr, "warn: provider %s: %v\n", d.Name, err)
			continue
		}
		if err := reg.Register(p, d.Force); err != nil {
			fmt.Fprintf(os.Stderr, "warn: register %s: %v\n", d.Name, err)
		}
	}

	app.Store = st
	app.Blobs = store.NewBlobs(blobsDir)
	app.Registry = reg
	return app, nil
}

// Execute is the entrypoint called from cmd/uta/main.go.
func Execute() {
	root := &cobra.Command{
		Use:           "uta",
		Short:         "unleash the agents — a CLI agent orchestrator",
		Version:       version.String(),
		SilenceUsage:  true,
		SilenceErrors: false,
	}
	root.PersistentFlags().String("log-level", "info", "log level: debug|info|warn|error")
	root.PersistentFlags().Bool("no-color", false, "disable ANSI colors")

	root.AddCommand(newDoctorCmd())
	root.AddCommand(newProvidersCmd())
	root.AddCommand(newRunCmd())
	root.AddCommand(newResumeCmd())
	root.AddCommand(newSessionsCmd())
	root.AddCommand(newTrajectoryCmd())
	root.AddCommand(newInitCmd())
	root.AddCommand(newExportDBCmd())
	root.AddCommand(newImportCmd())
	root.AddCommand(newProjectCmd())
	root.AddCommand(newCtxCmd())
	root.AddCommand(newAuditCmd())

	if err := root.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}
