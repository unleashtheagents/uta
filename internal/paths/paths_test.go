package paths

import (
	"os"
	"path/filepath"
	"testing"
)

func TestHome_UTAHomeOverride(t *testing.T) {
	dir := t.TempDir()
	custom := filepath.Join(dir, "custom-uta")
	t.Setenv("UTA_HOME", custom)

	got, err := Home()
	if err != nil {
		t.Fatalf("Home() error: %v", err)
	}
	if got != custom {
		t.Fatalf("Home() = %q, want %q", got, custom)
	}
	if st, err := os.Stat(custom); err != nil || !st.IsDir() {
		t.Fatalf("expected %s to be a created directory: err=%v", custom, err)
	}
}

func TestHome_DefaultsToUserHomeDotUta(t *testing.T) {
	fakeHome := t.TempDir()
	t.Setenv("UTA_HOME", "")
	t.Setenv("HOME", fakeHome)

	got, err := Home()
	if err != nil {
		t.Fatalf("Home() error: %v", err)
	}
	want := filepath.Join(fakeHome, ".uta")
	if got != want {
		t.Fatalf("Home() = %q, want %q", got, want)
	}
	if st, err := os.Stat(want); err != nil || !st.IsDir() {
		t.Fatalf("expected %s to be a created directory: err=%v", want, err)
	}
}

func TestDB(t *testing.T) {
	got := DB("/some/state")
	want := filepath.Join("/some/state", "uta.db")
	if got != want {
		t.Fatalf("DB() = %q, want %q", got, want)
	}
}

func TestBlobs_CreatesDir(t *testing.T) {
	state := t.TempDir()
	got, err := Blobs(state)
	if err != nil {
		t.Fatalf("Blobs() error: %v", err)
	}
	want := filepath.Join(state, "blobs")
	if got != want {
		t.Fatalf("Blobs() = %q, want %q", got, want)
	}
	if st, err := os.Stat(got); err != nil || !st.IsDir() {
		t.Fatalf("expected %s to be a created directory: err=%v", got, err)
	}
}

func TestProvidersDir_CreatesDir(t *testing.T) {
	home := t.TempDir()
	got, err := ProvidersDir(home)
	if err != nil {
		t.Fatalf("ProvidersDir() error: %v", err)
	}
	want := filepath.Join(home, "providers")
	if got != want {
		t.Fatalf("ProvidersDir() = %q, want %q", got, want)
	}
	if st, err := os.Stat(got); err != nil || !st.IsDir() {
		t.Fatalf("expected %s to be a created directory: err=%v", got, err)
	}
}

func TestProjectStateDir(t *testing.T) {
	got := ProjectStateDir("/some/project")
	want := filepath.Join("/some/project", ".uta")
	if got != want {
		t.Fatalf("ProjectStateDir() = %q, want %q", got, want)
	}
}

func TestProjectContextDir_CreatesDir(t *testing.T) {
	root := t.TempDir()
	got, err := ProjectContextDir(root)
	if err != nil {
		t.Fatalf("ProjectContextDir() error: %v", err)
	}
	want := filepath.Join(root, ".uta", "context")
	if got != want {
		t.Fatalf("ProjectContextDir() = %q, want %q", got, want)
	}
	if st, err := os.Stat(got); err != nil || !st.IsDir() {
		t.Fatalf("expected %s to be a created directory: err=%v", got, err)
	}
}

// resolveAbs makes test paths canonical so comparisons survive macOS's
// /var -> /private/var symlink (t.TempDir lives under /var on darwin).
func resolveAbs(t *testing.T, p string) string {
	t.Helper()
	r, err := filepath.EvalSymlinks(p)
	if err != nil {
		t.Fatalf("EvalSymlinks(%q): %v", p, err)
	}
	return r
}

func TestFindProjectRoot_FindsDotUtaInCwd(t *testing.T) {
	// Use a HOME outside the test workspace so the home-boundary check
	// can't interfere.
	t.Setenv("HOME", t.TempDir())

	root := resolveAbs(t, t.TempDir())
	if err := os.Mkdir(filepath.Join(root, ".uta"), 0o755); err != nil {
		t.Fatalf("mkdir .uta: %v", err)
	}

	got, ok := FindProjectRoot(root)
	if !ok {
		t.Fatalf("FindProjectRoot(%q) = _, false; want true", root)
	}
	if resolveAbs(t, got) != root {
		t.Fatalf("FindProjectRoot(%q) = %q, want %q", root, got, root)
	}
}

func TestFindProjectRoot_WalksUpToFindDotUta(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	root := resolveAbs(t, t.TempDir())
	if err := os.Mkdir(filepath.Join(root, ".uta"), 0o755); err != nil {
		t.Fatalf("mkdir .uta: %v", err)
	}
	deep := filepath.Join(root, "a", "b", "c")
	if err := os.MkdirAll(deep, 0o755); err != nil {
		t.Fatalf("mkdir deep: %v", err)
	}

	got, ok := FindProjectRoot(deep)
	if !ok {
		t.Fatalf("FindProjectRoot(%q) = _, false; want true", deep)
	}
	if resolveAbs(t, got) != root {
		t.Fatalf("FindProjectRoot(%q) = %q, want %q", deep, got, root)
	}
}

func TestFindProjectRoot_NotFoundReturnsFalse(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	dir := resolveAbs(t, t.TempDir())
	// No .uta anywhere in or above dir within the test workspace.
	got, ok := FindProjectRoot(dir)
	if ok {
		t.Fatalf("FindProjectRoot(%q) = (%q, true); want (_, false)", dir, got)
	}
}

func TestFindProjectRoot_StopsAtUserHome(t *testing.T) {
	// Simulate the case where the global ~/.uta exists at HOME: we must
	// NOT treat it as a project root.
	home := resolveAbs(t, t.TempDir())
	t.Setenv("HOME", home)

	if err := os.Mkdir(filepath.Join(home, ".uta"), 0o755); err != nil {
		t.Fatalf("mkdir ~/.uta: %v", err)
	}
	sub := filepath.Join(home, "sub", "dir")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatalf("mkdir sub: %v", err)
	}

	got, ok := FindProjectRoot(sub)
	if ok {
		t.Fatalf("FindProjectRoot(%q) = (%q, true); want (_, false) — home dir must be a boundary", sub, got)
	}
}

func TestFindProjectRoot_ProjectDeeperThanHomeIsFound(t *testing.T) {
	// A real project below HOME (with its own .uta) should still be found,
	// because the walk hits the project's .uta before reaching HOME.
	home := resolveAbs(t, t.TempDir())
	t.Setenv("HOME", home)

	project := filepath.Join(home, "code", "myproj")
	if err := os.MkdirAll(filepath.Join(project, ".uta"), 0o755); err != nil {
		t.Fatalf("mkdir project/.uta: %v", err)
	}
	sub := filepath.Join(project, "pkg", "sub")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatalf("mkdir sub: %v", err)
	}

	got, ok := FindProjectRoot(sub)
	if !ok {
		t.Fatalf("FindProjectRoot(%q) = _, false; want true", sub)
	}
	if resolveAbs(t, got) != project {
		t.Fatalf("FindProjectRoot(%q) = %q, want %q", sub, got, project)
	}
}

func TestFindProjectRoot_EmptyStartUsesCwd(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	root := resolveAbs(t, t.TempDir())
	if err := os.Mkdir(filepath.Join(root, ".uta"), 0o755); err != nil {
		t.Fatalf("mkdir .uta: %v", err)
	}
	deep := filepath.Join(root, "x", "y")
	if err := os.MkdirAll(deep, 0o755); err != nil {
		t.Fatalf("mkdir deep: %v", err)
	}

	prev, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(prev) })
	if err := os.Chdir(deep); err != nil {
		t.Fatalf("chdir: %v", err)
	}

	got, ok := FindProjectRoot("")
	if !ok {
		t.Fatalf("FindProjectRoot(\"\") = _, false; want true")
	}
	if resolveAbs(t, got) != root {
		t.Fatalf("FindProjectRoot(\"\") = %q, want %q", got, root)
	}
}

func TestWriteFileAtomic_WritesAndRenames(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "final.json")
	payload := []byte(`{"ok":true}`)
	if err := WriteFileAtomic(path, payload, 0o644); err != nil {
		t.Fatalf("WriteFileAtomic: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(got) != string(payload) {
		t.Fatalf("content mismatch: got %q want %q", string(got), string(payload))
	}
	// No leftover tmp files.
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("expected only the final file, got %v", names)
	}
}

func TestWriteFileAtomic_RejectsMissingDir(t *testing.T) {
	// Sanity check that the function bubbles up failures from the underlying
	// filesystem rather than silently succeeding.
	path := filepath.Join(t.TempDir(), "no", "such", "dir", "file.json")
	if err := WriteFileAtomic(path, []byte("x"), 0o644); err == nil {
		t.Fatalf("expected error writing to missing parent dir, got nil")
	}
}

func TestFindProjectRoot_IgnoresDotUtaFile(t *testing.T) {
	// .uta must be a *directory* — a regular file by that name should not
	// trick the walker.
	t.Setenv("HOME", t.TempDir())

	dir := resolveAbs(t, t.TempDir())
	if err := os.WriteFile(filepath.Join(dir, ".uta"), []byte("not a dir"), 0o644); err != nil {
		t.Fatalf("write .uta file: %v", err)
	}

	if got, ok := FindProjectRoot(dir); ok {
		t.Fatalf("FindProjectRoot(%q) = (%q, true); want (_, false) — .uta file should not count", dir, got)
	}
}
