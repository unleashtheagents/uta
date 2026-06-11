package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/unleashtheagents/uta/internal/config"
)

// runProject builds a fresh project command tree and executes it. Returns
// stdout, stderr, and the executor error.
func runProject(t *testing.T, args ...string) (string, string, error) {
	t.Helper()
	cmd := newProjectCmd()
	var out, errBuf bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errBuf)
	cmd.SetArgs(args)
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	err := cmd.ExecuteContext(context.Background())
	return out.String(), errBuf.String(), err
}

// setupProjectFixture creates an isolated HOME and UTA global home, then
// chdir's into a fresh temp dir intended to host a project. UTA_HOME is left
// empty so newApp() performs project auto-detection; HOME points at a
// throwaway tempdir so the walker can't reach the user's real home. Returns
// the chdir target (already EvalSymlinks-resolved) and the global home.
func setupProjectFixture(t *testing.T) (cwd, globalHome string) {
	t.Helper()
	// paths.Home() falls back to $HOME/.uta when UTA_HOME is empty; point
	// HOME at a tempdir so global state lands somewhere the test controls.
	homeBase := t.TempDir()
	t.Setenv("HOME", homeBase)
	t.Setenv("UTA_HOME", "")
	globalHome = homeBase

	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}
	prev, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(prev) })
	return dir, filepath.Join(globalHome, ".uta")
}

func TestProjectInit_CreatesWorkspace(t *testing.T) {
	dir, _ := setupProjectFixture(t)

	out, _, err := runProject(t, "init", "-n", "demo")
	if err != nil {
		t.Fatalf("project init: %v", err)
	}

	stateDir := filepath.Join(dir, ".uta")
	for _, sub := range []string{"", "blobs", "context"} {
		p := filepath.Join(stateDir, sub)
		st, err := os.Stat(p)
		if err != nil {
			t.Fatalf("expected %s to exist: %v", p, err)
		}
		if !st.IsDir() {
			t.Errorf("%s: want dir, got file", p)
		}
	}
	if _, err := os.Stat(filepath.Join(stateDir, "project.yaml")); err != nil {
		t.Errorf("project.yaml not written: %v", err)
	}
	if _, err := os.Stat(filepath.Join(stateDir, "uta.db")); err != nil {
		t.Errorf("uta.db not created: %v", err)
	}

	loaded, err := config.LoadProject(dir)
	if err != nil {
		t.Fatalf("LoadProject: %v", err)
	}
	if loaded.Name != "demo" {
		t.Errorf("project name = %q; want %q", loaded.Name, "demo")
	}

	for _, want := range []string{"initialized project", "demo", dir, "next steps:"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
}

func TestProjectInit_DefaultsNameToBasename(t *testing.T) {
	dir, _ := setupProjectFixture(t)

	if _, _, err := runProject(t, "init"); err != nil {
		t.Fatalf("project init: %v", err)
	}
	loaded, err := config.LoadProject(dir)
	if err != nil {
		t.Fatalf("LoadProject: %v", err)
	}
	if loaded.Name != filepath.Base(dir) {
		t.Errorf("default name = %q; want basename %q", loaded.Name, filepath.Base(dir))
	}
}

func TestProjectInit_RefusesReinitWithoutForce(t *testing.T) {
	dir, _ := setupProjectFixture(t)

	if _, _, err := runProject(t, "init"); err != nil {
		t.Fatalf("first init: %v", err)
	}
	// Touch project.yaml to confirm a refused re-init leaves it alone.
	yamlPath := filepath.Join(dir, ".uta", "project.yaml")
	original, err := os.ReadFile(yamlPath)
	if err != nil {
		t.Fatalf("read project.yaml: %v", err)
	}

	_, _, err = runProject(t, "init")
	if err == nil {
		t.Fatal("second init: want error, got nil")
	}
	if !strings.Contains(err.Error(), "already exists") {
		t.Errorf("error = %v; want 'already exists'", err)
	}
	got, err := os.ReadFile(yamlPath)
	if err != nil {
		t.Fatalf("re-read project.yaml: %v", err)
	}
	if !bytes.Equal(got, original) {
		t.Errorf("refused re-init mutated project.yaml")
	}
}

func TestProjectInit_ForceReinitializesExisting(t *testing.T) {
	dir, _ := setupProjectFixture(t)

	if _, _, err := runProject(t, "init", "-n", "first"); err != nil {
		t.Fatalf("first init: %v", err)
	}
	if _, _, err := runProject(t, "init", "-n", "second", "--force"); err != nil {
		t.Fatalf("forced re-init: %v", err)
	}
	loaded, err := config.LoadProject(dir)
	if err != nil {
		t.Fatalf("LoadProject: %v", err)
	}
	if loaded.Name != "second" {
		t.Errorf("forced re-init: name = %q; want %q", loaded.Name, "second")
	}
}

func TestProjectInit_RecordsInGlobalIndex(t *testing.T) {
	dir, globalHome := setupProjectFixture(t)

	if _, _, err := runProject(t, "init"); err != nil {
		t.Fatalf("project init: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(globalHome, "projects.index"))
	if err != nil {
		t.Fatalf("read index: %v", err)
	}
	if !strings.Contains(string(data), dir) {
		t.Errorf("index missing %q:\n%s", dir, data)
	}
}

func TestProjectInfo_OutsideProjectReportsClearly(t *testing.T) {
	// Isolate HOME so no parent .uta/ exists. Leave UTA_HOME empty so
	// newApp() goes through the project-detection branch — but with no
	// .uta in the cwd or any ancestor up to HOME, detection fails and
	// `info` should surface that fact instead of opening a stub project.
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("UTA_HOME", "")
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}
	prev, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(prev) })

	_, _, err = runProject(t, "info")
	if err == nil {
		t.Fatal("info outside project: want error, got nil")
	}
	if !strings.Contains(err.Error(), "not inside a project") {
		t.Errorf("error = %v; want 'not inside a project'", err)
	}
}

func TestProjectInfo_InsideProjectPrintsDetails(t *testing.T) {
	dir, _ := setupProjectFixture(t)

	if _, _, err := runProject(t, "init", "-n", "alpha"); err != nil {
		t.Fatalf("project init: %v", err)
	}

	out, _, err := runProject(t, "info")
	if err != nil {
		t.Fatalf("project info: %v", err)
	}
	for _, want := range []string{"alpha", dir, "state dir:", "context:", "schema:"} {
		if !strings.Contains(out, want) {
			t.Errorf("info output missing %q:\n%s", want, out)
		}
	}
}

func TestProjectInfo_JSON(t *testing.T) {
	dir, _ := setupProjectFixture(t)

	if _, _, err := runProject(t, "init", "-n", "beta"); err != nil {
		t.Fatalf("project init: %v", err)
	}

	out, _, err := runProject(t, "info", "--json")
	if err != nil {
		t.Fatalf("project info --json: %v", err)
	}
	var info map[string]any
	if err := json.Unmarshal([]byte(out), &info); err != nil {
		t.Fatalf("info JSON parse: %v\n%s", err, out)
	}
	if info["name"] != "beta" {
		t.Errorf("info[name] = %v; want beta", info["name"])
	}
	if info["root"] != dir {
		t.Errorf("info[root] = %v; want %s", info["root"], dir)
	}
}

func TestProjectList_EmptyMessage(t *testing.T) {
	globalHome := t.TempDir()
	t.Setenv("UTA_HOME", globalHome)

	out, _, err := runProject(t, "list")
	if err != nil {
		t.Fatalf("project list: %v", err)
	}
	if !strings.Contains(out, "no projects recorded yet") {
		t.Errorf("empty list output should mention 'no projects recorded yet':\n%s", out)
	}
}

func TestProjectList_ShowsRecordedProjectsWithStatus(t *testing.T) {
	globalHome := t.TempDir()
	t.Setenv("UTA_HOME", globalHome)

	// One project that still exists on disk and one whose .uta/ has been
	// removed — list should mark them ok vs missing accordingly.
	good, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}
	if err := config.SaveProject(good, &config.Project{Name: "good-one"}); err != nil {
		t.Fatalf("SaveProject good: %v", err)
	}
	gone, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}
	// gone never gets a .uta/ — its row should report status=missing and
	// name falls back to basename(path).

	idx := filepath.Join(globalHome, "projects.index")
	if err := os.WriteFile(idx, []byte(good+"\n"+gone+"\n"), 0o644); err != nil {
		t.Fatalf("write index: %v", err)
	}

	out, _, err := runProject(t, "list")
	if err != nil {
		t.Fatalf("project list: %v", err)
	}
	for _, want := range []string{"good-one", "ok", filepath.Base(gone), "missing"} {
		if !strings.Contains(out, want) {
			t.Errorf("list output missing %q:\n%s", want, out)
		}
	}
}

func TestProjectList_JSON(t *testing.T) {
	globalHome := t.TempDir()
	t.Setenv("UTA_HOME", globalHome)

	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}
	if err := config.SaveProject(root, &config.Project{Name: "json-proj"}); err != nil {
		t.Fatalf("SaveProject: %v", err)
	}
	if err := os.WriteFile(filepath.Join(globalHome, "projects.index"), []byte(root+"\n"), 0o644); err != nil {
		t.Fatalf("write index: %v", err)
	}

	out, _, err := runProject(t, "list", "--json")
	if err != nil {
		t.Fatalf("project list --json: %v", err)
	}
	var rows []map[string]any
	if err := json.Unmarshal([]byte(out), &rows); err != nil {
		t.Fatalf("list JSON parse: %v\n%s", err, out)
	}
	if len(rows) != 1 {
		t.Fatalf("rows = %d; want 1\n%s", len(rows), out)
	}
	if rows[0]["name"] != "json-proj" {
		t.Errorf("row name = %v; want json-proj", rows[0]["name"])
	}
	if rows[0]["root"] != root {
		t.Errorf("row root = %v; want %s", rows[0]["root"], root)
	}
	if rows[0]["ok"] != true {
		t.Errorf("row ok = %v; want true", rows[0]["ok"])
	}
}

// TestAppendProjectIndex_CreatesFile covers the cold-start path: no
// projects.index exists yet, the first call should create it with a single
// trailing-newline entry.
func TestAppendProjectIndex_CreatesFile(t *testing.T) {
	home := t.TempDir()
	if err := appendProjectIndex(home, "/projects/alpha"); err != nil {
		t.Fatalf("appendProjectIndex: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(home, "projects.index"))
	if err != nil {
		t.Fatalf("read index: %v", err)
	}
	if string(data) != "/projects/alpha\n" {
		t.Errorf("index = %q; want %q", data, "/projects/alpha\n")
	}
}

// TestAppendProjectIndex_DedupesExistingEntries: calling twice with the
// same root must leave the file with a single line — the second call is a
// no-op (covered by the early-return inside the function).
func TestAppendProjectIndex_DedupesExistingEntries(t *testing.T) {
	home := t.TempDir()
	root := "/projects/dup"
	for i := 0; i < 3; i++ {
		if err := appendProjectIndex(home, root); err != nil {
			t.Fatalf("appendProjectIndex #%d: %v", i, err)
		}
	}
	data, err := os.ReadFile(filepath.Join(home, "projects.index"))
	if err != nil {
		t.Fatalf("read index: %v", err)
	}
	if string(data) != root+"\n" {
		t.Errorf("dedup failed; index = %q", data)
	}
}

// TestAppendProjectIndex_PreservesPriorEntries: appending a new root after
// pre-existing rows should append exactly one line and leave the originals
// untouched. This indirectly validates origLen tracking — the post-write
// file size equals the original size plus the new line, which is the
// invariant the partial-write recovery path is designed to preserve.
func TestAppendProjectIndex_PreservesPriorEntries(t *testing.T) {
	home := t.TempDir()
	idx := filepath.Join(home, "projects.index")
	existing := []byte("/projects/a\n/projects/b\n")
	if err := os.WriteFile(idx, existing, 0o644); err != nil {
		t.Fatalf("seed index: %v", err)
	}
	if err := appendProjectIndex(home, "/projects/c"); err != nil {
		t.Fatalf("appendProjectIndex: %v", err)
	}
	got, err := os.ReadFile(idx)
	if err != nil {
		t.Fatalf("read index: %v", err)
	}
	want := "/projects/a\n/projects/b\n/projects/c\n"
	if string(got) != want {
		t.Errorf("index = %q; want %q", got, want)
	}
}

// TestAppendProjectIndex_OpenFileErrorLeavesIndexUntouched: when the index
// path is unusable (here, a directory in place of the expected file), the
// function returns an error without mutating any sibling state.
func TestAppendProjectIndex_OpenFileErrorLeavesIndexUntouched(t *testing.T) {
	home := t.TempDir()
	idx := filepath.Join(home, "projects.index")
	if err := os.Mkdir(idx, 0o755); err != nil {
		t.Fatalf("mkdir conflicting dir: %v", err)
	}
	err := appendProjectIndex(home, "/projects/blocked")
	if err == nil {
		t.Fatal("appendProjectIndex with directory at index path: want error, got nil")
	}
	st, statErr := os.Stat(idx)
	if statErr != nil {
		t.Fatalf("stat idx: %v", statErr)
	}
	if !st.IsDir() {
		t.Errorf("index path should still be a directory; appendProjectIndex must not replace it")
	}
}
