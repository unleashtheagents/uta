// Package cli wires cobra commands to the engine. Each command is a thin
// shell; all behavior lives in internal/{engine,provider,store,trajectory} so
// a future TUI or MCP-server mode can reuse it without going through cobra.
package cli

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/unleashtheagents/uta/internal/config"
	"github.com/unleashtheagents/uta/internal/orgstate"
	"github.com/unleashtheagents/uta/internal/paths"
	"github.com/unleashtheagents/uta/internal/provider"
	"github.com/unleashtheagents/uta/internal/provider/builtin"
	"github.com/unleashtheagents/uta/internal/provider/declarative"
	"github.com/unleashtheagents/uta/internal/researchdb"
	"github.com/unleashtheagents/uta/internal/store"
	"github.com/unleashtheagents/uta/internal/version"
)

// exitError signals that the program should terminate with a specific exit
// code without further diagnostic output. Commands return this in place of
// calling os.Exit directly so that deferred cleanup (store close, bus
// shutdown, signal-context cancellation) runs before the process exits.
// Execute unwraps it via errors.As and calls os.Exit with the carried code.
type exitError struct{ code int }

func (e *exitError) Error() string { return fmt.Sprintf("exit %d", e.code) }

// exitWith returns a sentinel error that propagates a specific exit code up
// to Execute. The caller is responsible for printing any user-facing
// diagnostic before returning.
func exitWith(code int) error { return &exitError{code: code} }

// App is the per-invocation context: where uta is storing state, the open
// DB, the blob store, and the provider registry. Built once per command via
// newApp(). If the cwd is inside a project (a .uta/project.yaml exists
// somewhere up the tree), the App is project-scoped — DB and blobs come from
// the project state dir, not the global home. Providers always come from the
// global home (they're user-wide, not project-specific).
//
// User-defined personas (~/.uta/personas/*.yaml) are loaded at startup
// alongside provider descriptors.
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

	Store        *store.Store
	Blobs        *store.Blobs
	Registry     *provider.Registry
	UserPersonas []*config.Persona
}

func (a *App) Close() {
	if a.Store != nil {
		a.Store.Close()
	}
}

// InProject reports whether this invocation is scoped to a project.
func (a *App) InProject() bool { return a.ProjectRoot != "" }

// ProjectSubtaskEnv returns the project-scoped env vars every subtask
// should see when uta is running inside a project. Returns nil when not
// in a project so callers can append unconditionally. Keep this in sync
// with the orgstate / researchdb package docs, which advertise the
// $UTA_ORG_STATE and $UTA_RESEARCH_DB names to persona authors.
func (a *App) ProjectSubtaskEnv() []string {
	if !a.InProject() {
		return nil
	}
	return []string{
		"UTA_PROJECT_ROOT=" + a.ProjectRoot,
		"UTA_CONTEXT_DIR=" + a.ContextDir,
		"UTA_PROJECT_NAME=" + a.ProjectName,
		"UTA_ORG_STATE=" + orgstate.Path(a.ContextDir),
		"UTA_RESEARCH_DB=" + researchdb.Path(a.ContextDir),
	}
}

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
	if err := reg.Register(&builtin.Claude{}, false); err != nil {
		st.Close()
		return nil, err
	}
	if err := reg.Register(&builtin.Gemini{}, false); err != nil {
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

	// User-defined personas under <globalHome>/personas/.
	personasDir, _ := config.PersonasDir(globalHome)
	personas, personaErrs := config.LoadPersonasDir(personasDir)
	for _, e := range personaErrs {
		fmt.Fprintln(os.Stderr, "warn: persona:", e)
	}

	app.Store = st
	app.Blobs = store.NewBlobs(blobsDir)
	app.Registry = reg
	app.UserPersonas = personas
	return app, nil
}

// Execute is the entrypoint called from cmd/uta/main.go.
func Execute() {
	root := &cobra.Command{
		Use:           "uta",
		Short:         "unleash the agents — a CLI agent orchestrator",
		Version:       version.String(),
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.PersistentFlags().String("log-level", "info", "log level: debug|info|warn|error")
	root.PersistentFlags().Bool("no-color", false, "disable ANSI colors")
	// Note: no backticks in the usage string — cobra renders backticked text
	// as the flag's type placeholder ("--mode uta mode list").
	root.PersistentFlags().String("mode", "", "MissionProfile name (see 'uta mode list'). When set, the profile's env is merged in and its allowed_tools restricts --pre-approve.")

	// Group commands so `uta --help` reads as a guided menu instead of a
	// flat 28-entry list. addTo assigns the group id and registers.
	addTo := func(groupID string, cmds ...*cobra.Command) {
		for _, c := range cmds {
			c.GroupID = groupID
			root.AddCommand(c)
		}
	}
	root.AddGroup(
		&cobra.Group{ID: "start", Title: "Getting Started:"},
		&cobra.Group{ID: "orchestrate", Title: "Run & Orchestrate:"},
		&cobra.Group{ID: "inspect", Title: "Inspect & Analyze:"},
		&cobra.Group{ID: "workspace", Title: "Workspace & Steering:"},
		&cobra.Group{ID: "data", Title: "Memory & Data:"},
		&cobra.Group{ID: "integrate", Title: "Integrations:"},
	)
	addTo("start", newHelloCmd(), newDoctorCmd(), newInitCmd(), newProvidersCmd())
	addTo("orchestrate", newRunCmd(), newResumeCmd(), newImproveCmd(), newAuditCmd(), newEvalCmd(), newShadowCmd(), newHITLCmd())
	addTo("inspect", newSessionsCmd(), newTrajectoryCmd(), newPerfCmd(), newDashCmd())
	addTo("workspace", newProjectCmd(), newCtxCmd(), newThreadCmd(), newModeCmd(), newVibeCmd())
	addTo("data", newMemoryCmd(), newRecallCmd(), newIdeasCmd(), newExportDBCmd(), newImportCmd())
	addTo("integrate", newServeCmd())
	root.SetHelpCommandGroupID("start")
	root.SetCompletionCommandGroupID("integrate")

	if err := root.Execute(); err != nil {
		var ee *exitError
		if errors.As(err, &ee) {
			os.Exit(ee.code)
		}
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}
