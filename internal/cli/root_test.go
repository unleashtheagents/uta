package cli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/unleashtheagents/uta/internal/config"
	"github.com/unleashtheagents/uta/internal/orgstate"
	"github.com/unleashtheagents/uta/internal/paths"
	"github.com/unleashtheagents/uta/internal/researchdb"
)

// TestProjectSubtaskEnv_OutsideProject: a non-project App must not leak
// any project-context env vars into the subtask. Otherwise a subtask run
// outside a project would see UTA_RESEARCH_DB pointing at a phantom
// path under /RESEARCH_DATABASE.md.
func TestProjectSubtaskEnv_OutsideProject(t *testing.T) {
	app := &App{}
	if env := app.ProjectSubtaskEnv(); env != nil {
		t.Errorf("non-project App should return nil env, got %v", env)
	}
}

// TestProjectSubtaskEnv_InProject pins the full set of project-context
// env vars the run loop hands every subtask. The persona docs in
// internal/orgstate and internal/researchdb advertise these names to
// persona authors; this test fails loudly if any name is renamed,
// dropped, or pointed at the wrong path. The audit findings flagged the
// research-DB env var as missing — this is the integration gate.
func TestProjectSubtaskEnv_InProject(t *testing.T) {
	app := &App{
		ProjectRoot: "/tmp/proj",
		ProjectName: "demo",
		ContextDir:  "/tmp/proj/.uta/context",
	}
	env := app.ProjectSubtaskEnv()
	want := map[string]string{
		"UTA_PROJECT_ROOT": "/tmp/proj",
		"UTA_CONTEXT_DIR":  "/tmp/proj/.uta/context",
		"UTA_PROJECT_NAME": "demo",
		"UTA_ORG_STATE":    orgstate.Path("/tmp/proj/.uta/context"),
		"UTA_RESEARCH_DB":  researchdb.Path("/tmp/proj/.uta/context"),
	}
	got := make(map[string]string, len(env))
	for _, kv := range env {
		i := strings.IndexByte(kv, '=')
		if i < 0 {
			t.Fatalf("malformed env entry %q", kv)
		}
		got[kv[:i]] = kv[i+1:]
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %q, want %q", k, got[k], v)
		}
	}
	if len(got) != len(want) {
		t.Errorf("env keys = %v, want %v", got, want)
	}
}

// chdirTo chdirs into dir for the duration of the test and registers a
// cleanup that restores the previous cwd. Returns the EvalSymlinks-resolved
// path so callers can compare against paths that go through the OS-level
// symlink machinery (macOS's /var -> /private/var).
func chdirTo(t *testing.T, dir string) string {
	t.Helper()
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatalf("EvalSymlinks(%q): %v", dir, err)
	}
	prev, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	if err := os.Chdir(resolved); err != nil {
		t.Fatalf("chdir(%q): %v", resolved, err)
	}
	t.Cleanup(func() { _ = os.Chdir(prev) })
	return resolved
}

// TestNewApp_GlobalScopedOutsideProject: when nothing in or above the cwd
// contains a .uta/ directory, newApp() must fall back to the GLOBAL home
// for state — the DB and blobs live under $HOME/.uta, not under the cwd.
func TestNewApp_GlobalScopedOutsideProject(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("UTA_HOME", "")
	chdirTo(t, t.TempDir())

	app, err := newApp(context.Background())
	if err != nil {
		t.Fatalf("newApp: %v", err)
	}
	defer app.Close()

	wantHome := filepath.Join(home, ".uta")
	if app.GlobalHome != wantHome {
		t.Errorf("GlobalHome = %q, want %q", app.GlobalHome, wantHome)
	}
	if app.StateDir != wantHome {
		t.Errorf("StateDir = %q, want %q (no project => fall back to global)", app.StateDir, wantHome)
	}
	if app.ProjectRoot != "" {
		t.Errorf("ProjectRoot = %q, want empty", app.ProjectRoot)
	}
	if app.ContextDir != "" {
		t.Errorf("ContextDir = %q, want empty", app.ContextDir)
	}
	if app.InProject() {
		t.Errorf("InProject() = true, want false")
	}
	// DB must materialize under the global home, not the cwd.
	if _, err := os.Stat(paths.DB(wantHome)); err != nil {
		t.Errorf("expected global DB at %s: %v", paths.DB(wantHome), err)
	}
}

// TestNewApp_ProjectScopedInsideProject: when the cwd is inside a
// directory containing .uta/project.yaml, newApp() must scope StateDir to
// the project (so DB and blobs land under <root>/.uta/, not the global
// home) and populate ProjectRoot / ProjectName / ContextDir accordingly.
func TestNewApp_ProjectScopedInsideProject(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("UTA_HOME", "")

	projectRoot := chdirTo(t, t.TempDir())
	if err := config.SaveProject(projectRoot, &config.Project{Name: "demo-proj"}); err != nil {
		t.Fatalf("SaveProject: %v", err)
	}

	app, err := newApp(context.Background())
	if err != nil {
		t.Fatalf("newApp: %v", err)
	}
	defer app.Close()

	wantState := filepath.Join(projectRoot, ".uta")
	wantContext := filepath.Join(wantState, "context")
	if app.ProjectRoot != projectRoot {
		t.Errorf("ProjectRoot = %q, want %q", app.ProjectRoot, projectRoot)
	}
	if app.ProjectName != "demo-proj" {
		t.Errorf("ProjectName = %q, want %q", app.ProjectName, "demo-proj")
	}
	if app.StateDir != wantState {
		t.Errorf("StateDir = %q, want %q (project scoped)", app.StateDir, wantState)
	}
	if app.ContextDir != wantContext {
		t.Errorf("ContextDir = %q, want %q", app.ContextDir, wantContext)
	}
	if app.GlobalHome != filepath.Join(home, ".uta") {
		t.Errorf("GlobalHome = %q, want %q (always global, never project-scoped)",
			app.GlobalHome, filepath.Join(home, ".uta"))
	}
	if !app.InProject() {
		t.Errorf("InProject() = false, want true")
	}
	// Project DB exists, and the global DB has NOT been created as a
	// side-effect — otherwise we'd silently fork the user's state into two
	// places every time they cd into a project.
	if _, err := os.Stat(paths.DB(wantState)); err != nil {
		t.Errorf("expected project DB at %s: %v", paths.DB(wantState), err)
	}
	if _, err := os.Stat(paths.DB(app.GlobalHome)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("global DB should NOT exist when running inside a project; stat err = %v", err)
	}
}

// TestNewApp_UTAHomeSkipsProjectDetection: setting $UTA_HOME pins state to
// that directory and bypasses the cwd walker entirely — even when the cwd
// IS inside a project. This preserves the v0.2 ergonomics for ad-hoc runs
// (the user has explicitly asked for "global mode").
func TestNewApp_UTAHomeSkipsProjectDetection(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	override, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}
	t.Setenv("UTA_HOME", override)

	// Make the cwd a real project; without the override this would scope
	// the App to it.
	projectRoot := chdirTo(t, t.TempDir())
	if err := config.SaveProject(projectRoot, &config.Project{Name: "should-be-ignored"}); err != nil {
		t.Fatalf("SaveProject: %v", err)
	}

	app, err := newApp(context.Background())
	if err != nil {
		t.Fatalf("newApp: %v", err)
	}
	defer app.Close()

	if app.GlobalHome != override {
		t.Errorf("GlobalHome = %q, want %q (UTA_HOME override)", app.GlobalHome, override)
	}
	if app.StateDir != override {
		t.Errorf("StateDir = %q, want %q (UTA_HOME must skip project detection)",
			app.StateDir, override)
	}
	if app.ProjectRoot != "" {
		t.Errorf("ProjectRoot = %q, want empty (UTA_HOME bypasses detection)", app.ProjectRoot)
	}
	if app.InProject() {
		t.Errorf("InProject() = true, want false (UTA_HOME override)")
	}
}

// TestNewApp_RegisterBuiltinProviders: newApp() must seed the registry
// with the built-in providers (claude, gemini). Tests in other commands
// rely on these being present after construction.
func TestNewApp_RegisterBuiltinProviders(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("UTA_HOME", "")
	chdirTo(t, t.TempDir())

	app, err := newApp(context.Background())
	if err != nil {
		t.Fatalf("newApp: %v", err)
	}
	defer app.Close()

	if app.Registry == nil {
		t.Fatal("Registry is nil; want built-in providers registered")
	}
	for _, name := range []string{"claude", "gemini"} {
		if _, ok := app.Registry.Get(name); !ok {
			t.Errorf("Registry.Get(%q) not registered", name)
		}
	}
}

// TestExitWith_CarriesCode pins the contract that Execute relies on: the
// sentinel error returned by exitWith must be unwrappable to an *exitError
// that surfaces the requested exit code. If this ever drifts, every
// "cleanly fail with a specific status" call site in the CLI starts
// printing the sentinel text instead of exiting silently.
func TestExitWith_CarriesCode(t *testing.T) {
	err := exitWith(42)
	var ee *exitError
	if !errors.As(err, &ee) {
		t.Fatalf("exitWith result not unwrappable to *exitError: %v", err)
	}
	if ee.code != 42 {
		t.Errorf("code = %d, want 42", ee.code)
	}
	if !strings.Contains(err.Error(), "42") {
		t.Errorf("Error() = %q, want it to mention the code", err.Error())
	}
}
