package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSaveProject_CreatesStateDirAndPopulatesDefaults(t *testing.T) {
	root := t.TempDir()
	// .uta does not exist yet — SaveProject should create it.
	if _, err := os.Stat(filepath.Join(root, ".uta")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("precondition: .uta should not exist yet, got err=%v", err)
	}

	p := &Project{}
	if err := SaveProject(root, p); err != nil {
		t.Fatalf("SaveProject: %v", err)
	}

	info, err := os.Stat(filepath.Join(root, ".uta", "project.yaml"))
	if err != nil {
		t.Fatalf("project.yaml not written: %v", err)
	}
	if info.Size() == 0 {
		t.Fatalf("project.yaml is empty")
	}

	// Defaults should have been filled in on the struct in place.
	if p.SchemaVersion != CurrentSchemaVersion {
		t.Errorf("SchemaVersion: got %d, want %d", p.SchemaVersion, CurrentSchemaVersion)
	}
	if p.CreatedAt.IsZero() {
		t.Errorf("CreatedAt: should have been set, got zero")
	}
	if p.Name != filepath.Base(root) {
		t.Errorf("Name: got %q, want %q", p.Name, filepath.Base(root))
	}
}

func TestSaveProject_PreservesProvidedFields(t *testing.T) {
	root := t.TempDir()
	when := time.Date(2024, 1, 2, 3, 4, 5, 0, time.UTC)
	p := &Project{
		Name:          "custom-name",
		CreatedAt:     when,
		SchemaVersion: CurrentSchemaVersion,
	}
	if err := SaveProject(root, p); err != nil {
		t.Fatalf("SaveProject: %v", err)
	}

	loaded, err := LoadProject(root)
	if err != nil {
		t.Fatalf("LoadProject: %v", err)
	}
	if loaded.Name != "custom-name" {
		t.Errorf("Name: got %q, want %q", loaded.Name, "custom-name")
	}
	if !loaded.CreatedAt.Equal(when) {
		t.Errorf("CreatedAt: got %v, want %v", loaded.CreatedAt, when)
	}
	if loaded.SchemaVersion != CurrentSchemaVersion {
		t.Errorf("SchemaVersion: got %d, want %d", loaded.SchemaVersion, CurrentSchemaVersion)
	}
	if loaded.Root != root {
		t.Errorf("Root: got %q, want %q", loaded.Root, root)
	}
}

func TestSaveProject_AtomicNoStaleTempFiles(t *testing.T) {
	root := t.TempDir()
	p := &Project{Name: "atomic"}
	if err := SaveProject(root, p); err != nil {
		t.Fatalf("SaveProject: %v", err)
	}

	// The temp file used for the atomic rename must not linger after success.
	entries, err := os.ReadDir(filepath.Join(root, ".uta"))
	if err != nil {
		t.Fatalf("read .uta: %v", err)
	}
	for _, e := range entries {
		name := e.Name()
		if name == "project.yaml" {
			continue
		}
		if strings.HasPrefix(name, "project-") && strings.HasSuffix(name, ".tmp") {
			t.Errorf("leftover temp file: %q", name)
		}
	}
}

func TestSaveProject_OverwritesExisting(t *testing.T) {
	root := t.TempDir()
	if err := SaveProject(root, &Project{Name: "first"}); err != nil {
		t.Fatalf("first SaveProject: %v", err)
	}
	if err := SaveProject(root, &Project{Name: "second"}); err != nil {
		t.Fatalf("second SaveProject: %v", err)
	}
	loaded, err := LoadProject(root)
	if err != nil {
		t.Fatalf("LoadProject: %v", err)
	}
	if loaded.Name != "second" {
		t.Errorf("Name after overwrite: got %q, want %q", loaded.Name, "second")
	}
}

func TestSaveProject_StateDirCreationError(t *testing.T) {
	// If .uta already exists as a regular file, MkdirAll will fail and
	// SaveProject must surface that error instead of silently overwriting.
	root := t.TempDir()
	blockingPath := filepath.Join(root, ".uta")
	if err := os.WriteFile(blockingPath, []byte("not a directory"), 0o644); err != nil {
		t.Fatalf("seed blocker: %v", err)
	}
	err := SaveProject(root, &Project{Name: "x"})
	if err == nil {
		t.Fatalf("SaveProject: want error, got nil")
	}
}

func TestLoadProject_Missing(t *testing.T) {
	root := t.TempDir()
	_, err := LoadProject(root)
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("LoadProject on empty root: got %v, want os.ErrNotExist", err)
	}
}

func TestLoadProject_Malformed(t *testing.T) {
	root := t.TempDir()
	stateDir := filepath.Join(root, ".uta")
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// Invalid YAML — unbalanced bracket plus a mismatched type for a field.
	if err := os.WriteFile(filepath.Join(stateDir, "project.yaml"), []byte("name: [unterminated\nschema_version: not-an-int\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	_, err := LoadProject(root)
	if err == nil {
		t.Fatalf("LoadProject: want parse error, got nil")
	}
	if errors.Is(err, os.ErrNotExist) {
		t.Fatalf("LoadProject: got os.ErrNotExist, want parse error")
	}
	if !strings.Contains(err.Error(), "parse") {
		t.Errorf("error should mention parse failure: %v", err)
	}
}

func TestLoadProject_EmptyFileYieldsZeroValueProject(t *testing.T) {
	// An empty file is valid YAML (null document). The current contract is
	// that LoadProject returns a zero-valued Project with Root populated;
	// callers use Validate() to catch the missing fields.
	root := t.TempDir()
	stateDir := filepath.Join(root, ".uta")
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(stateDir, "project.yaml"), []byte(""), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	p, err := LoadProject(root)
	if err != nil {
		t.Fatalf("LoadProject on empty file: %v", err)
	}
	if p.Root != root {
		t.Errorf("Root: got %q, want %q", p.Root, root)
	}
	if err := p.Validate(); err == nil {
		t.Errorf("Validate on empty project: want error, got nil")
	}
}

func TestProjectExists(t *testing.T) {
	root := t.TempDir()
	if ProjectExists(root) {
		t.Fatalf("ProjectExists on empty dir: got true, want false")
	}
	if err := SaveProject(root, &Project{Name: "x"}); err != nil {
		t.Fatalf("SaveProject: %v", err)
	}
	if !ProjectExists(root) {
		t.Fatalf("ProjectExists after SaveProject: got false, want true")
	}
}

func TestProject_Validate(t *testing.T) {
	cases := []struct {
		name    string
		p       Project
		wantErr bool
	}{
		{"ok", Project{Name: "good", SchemaVersion: CurrentSchemaVersion}, false},
		{"empty name", Project{Name: "", SchemaVersion: CurrentSchemaVersion}, true},
		{"whitespace name", Project{Name: "   ", SchemaVersion: CurrentSchemaVersion}, true},
		{"schema too low", Project{Name: "x", SchemaVersion: 0}, true},
		{"schema too high", Project{Name: "x", SchemaVersion: CurrentSchemaVersion + 1}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.p.Validate()
			if tc.wantErr && err == nil {
				t.Errorf("want error, got nil")
			}
			if !tc.wantErr && err != nil {
				t.Errorf("unexpected error: %v", err)
			}
		})
	}
}
