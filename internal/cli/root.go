// Package cli wires cobra commands to the engine. Each command is a thin
// shell; all behavior lives in internal/{engine,provider,store,trajectory} so
// a future TUI or MCP-server mode can reuse it without going through cobra.
package cli

import (
	"context"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/unleashtheagents/uta/internal/paths"
	"github.com/unleashtheagents/uta/internal/provider"
	"github.com/unleashtheagents/uta/internal/provider/builtin"
	"github.com/unleashtheagents/uta/internal/store"
	"github.com/unleashtheagents/uta/internal/version"
)

// App is the per-invocation context: the resolved UTA_HOME, the open store,
// and the provider registry. Built once per command via newApp().
type App struct {
	Home     string
	Store    *store.Store
	Blobs    *store.Blobs
	Registry *provider.Registry
}

func (a *App) Close() {
	if a.Store != nil {
		a.Store.Close()
	}
}

// newApp resolves paths, opens the store (running migrations if needed), and
// builds a registry seeded with the built-in providers.
func newApp(_ context.Context) (*App, error) {
	home, err := paths.Home()
	if err != nil {
		return nil, fmt.Errorf("resolve UTA_HOME: %w", err)
	}
	st, err := store.Open(paths.DB(home))
	if err != nil {
		return nil, fmt.Errorf("open store: %w", err)
	}
	blobsDir, err := paths.Blobs(home)
	if err != nil {
		st.Close()
		return nil, err
	}
	if _, err := paths.ProvidersDir(home); err != nil {
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
	// Declarative YAML providers (~/.uta/providers/*.yaml) load here in a later chunk.

	return &App{
		Home:     home,
		Store:    st,
		Blobs:    store.NewBlobs(blobsDir),
		Registry: reg,
	}, nil
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

	if err := root.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}
