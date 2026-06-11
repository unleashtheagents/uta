package cli

import (
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
)

func TestWritableCheck_OK(t *testing.T) {
	dir := t.TempDir()
	if err := writableCheck(dir); err != nil {
		t.Fatalf("writableCheck(%q) unexpected error: %v", dir, err)
	}
	// The probe file must be cleaned up afterwards.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("writableCheck left %d entries behind: %+v", len(entries), entries)
	}
}

func TestWritableCheck_MissingDir(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "does-not-exist")
	err := writableCheck(missing)
	if err == nil {
		t.Fatalf("writableCheck(%q) = nil; want error", missing)
	}
}

func TestWritableCheck_ReadOnlyDir(t *testing.T) {
	// chmod 0o500 is not enforced on Windows the same way, and root on
	// some systems bypasses permission bits. Skip rather than flake.
	if runtime.GOOS == "windows" {
		t.Skip("permission semantics differ on windows")
	}
	if os.Geteuid() == 0 {
		t.Skip("running as root bypasses permission checks")
	}
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatalf("chmod ro: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

	if err := writableCheck(dir); err == nil {
		t.Fatalf("writableCheck(read-only dir) = nil; want error")
	}
}

func TestCountEntries_Empty(t *testing.T) {
	dir := t.TempDir()
	n, err := countEntries(dir)
	if err != nil {
		t.Fatalf("countEntries: %v", err)
	}
	if n != 0 {
		t.Errorf("countEntries(empty) = %d; want 0", n)
	}
}

func TestCountEntries_Multiple(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"a", "b", "c"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o644); err != nil {
			t.Fatalf("seed %s: %v", name, err)
		}
	}
	// A nested directory should count as a single entry (top-level only).
	if err := os.Mkdir(filepath.Join(dir, "sub"), 0o755); err != nil {
		t.Fatalf("mkdir sub: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "sub", "hidden"), []byte("x"), 0o644); err != nil {
		t.Fatalf("seed nested: %v", err)
	}

	n, err := countEntries(dir)
	if err != nil {
		t.Fatalf("countEntries: %v", err)
	}
	if n != 4 {
		t.Errorf("countEntries = %d; want 4", n)
	}
}

func TestCountEntries_MissingDir(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "nope")
	n, err := countEntries(missing)
	if err == nil {
		t.Fatalf("countEntries(missing) = %d, nil; want error", n)
	}
	if !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("countEntries(missing) error = %v; want ErrNotExist", err)
	}
	if n != 0 {
		t.Errorf("countEntries(missing) count = %d; want 0 on error", n)
	}
}

// TestPathShadowCheck_DetectsForeignExecutable simulates the conda/pip
// shadowing failure mode: `uta` on PATH resolves to a different file
// than the running binary. The check must flag it.
func TestPathShadowCheck_DetectsForeignExecutable(t *testing.T) {
	dir := t.TempDir()
	foreign := filepath.Join(dir, "uta")
	if err := os.WriteFile(foreign, []byte("#!/usr/bin/env python3\nprint('old prototype')\n"), 0o755); err != nil {
		t.Fatalf("write foreign shim: %v", err)
	}
	orig := execLookPath
	execLookPath = func(name string) (string, error) {
		if name == "uta" {
			return foreign, nil
		}
		return orig(name)
	}
	t.Cleanup(func() { execLookPath = orig })

	self, resolved, shadowed := pathShadowCheck()
	if !shadowed {
		t.Fatalf("expected shadowed=true (self=%s resolved=%s)", self, resolved)
	}
	if resolved != foreign && resolved != evalSymlinks(foreign) {
		t.Errorf("resolved = %q, want the foreign shim %q", resolved, foreign)
	}
}

// TestPathShadowCheck_SelfViaSymlinkIsNotShadowed: a symlink chain to
// the SAME binary (the brew /opt/homebrew/bin/uta -> Cellar layout)
// must not be reported as shadowing.
func TestPathShadowCheck_SelfViaSymlinkIsNotShadowed(t *testing.T) {
	selfRaw, err := os.Executable()
	if err != nil {
		t.Skipf("os.Executable: %v", err)
	}
	dir := t.TempDir()
	link := filepath.Join(dir, "uta")
	if err := os.Symlink(selfRaw, link); err != nil {
		t.Skipf("symlink: %v", err)
	}
	orig := execLookPath
	execLookPath = func(name string) (string, error) {
		if name == "uta" {
			return link, nil
		}
		return orig(name)
	}
	t.Cleanup(func() { execLookPath = orig })

	self, resolved, shadowed := pathShadowCheck()
	if shadowed {
		t.Errorf("symlink to self flagged as shadowed (self=%s resolved=%s)", self, resolved)
	}
}

// TestPathShadowCheck_NoUtaOnPath: running via ./uta with nothing on
// PATH is not shadowing.
func TestPathShadowCheck_NoUtaOnPath(t *testing.T) {
	orig := execLookPath
	execLookPath = func(name string) (string, error) {
		return "", exec.ErrNotFound
	}
	t.Cleanup(func() { execLookPath = orig })

	_, resolved, shadowed := pathShadowCheck()
	if shadowed || resolved != "" {
		t.Errorf("no uta on PATH must not shadow (resolved=%q shadowed=%v)", resolved, shadowed)
	}
}
