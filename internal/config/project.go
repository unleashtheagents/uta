package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Project is the on-disk metadata stored at <root>/.uta/project.yaml.
type Project struct {
	// Name is the human-readable identifier. Defaults to basename(root) at
	// init time. Used for display only.
	Name string `yaml:"name"`
	// CreatedAt is the project-init timestamp (UTC).
	CreatedAt time.Time `yaml:"created_at"`
	// SchemaVersion is bumped whenever this struct's on-disk shape changes
	// in a way that requires migration.
	SchemaVersion int `yaml:"schema_version"`

	// Root is populated at load time (the directory containing .uta/). Not
	// serialized.
	Root string `yaml:"-"`
}

// CurrentSchemaVersion is what fresh-init projects record.
const CurrentSchemaVersion = 1

// LoadProject reads <root>/.uta/project.yaml. Returns os.ErrNotExist if the
// file isn't there (caller can decide whether to treat as a missing project
// or just a stub).
func LoadProject(root string) (*Project, error) {
	path := filepath.Join(root, ".uta", "project.yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var p Project
	if err := yaml.Unmarshal(data, &p); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	p.Root = root
	return &p, nil
}

// SaveProject writes p to <root>/.uta/project.yaml. p.Root is honored even
// if not set explicitly; if Root and root disagree, root wins.
func SaveProject(root string, p *Project) error {
	stateDir := filepath.Join(root, ".uta")
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		return err
	}
	if p.SchemaVersion == 0 {
		p.SchemaVersion = CurrentSchemaVersion
	}
	if p.CreatedAt.IsZero() {
		p.CreatedAt = time.Now().UTC()
	}
	if strings.TrimSpace(p.Name) == "" {
		p.Name = filepath.Base(root)
	}
	out, err := yaml.Marshal(p)
	if err != nil {
		return err
	}
	target := filepath.Join(stateDir, "project.yaml")
	tmp, err := os.CreateTemp(stateDir, "project-*.yaml.tmp")
	if err != nil {
		return fmt.Errorf("write %s: %w", target, err)
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(out); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return fmt.Errorf("write %s: %w", target, err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return fmt.Errorf("write %s: %w", target, err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("write %s: %w", target, err)
	}
	if err := os.Chmod(tmpName, 0o644); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("write %s: %w", target, err)
	}
	if err := os.Rename(tmpName, target); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("write %s: %w", target, err)
	}
	return nil
}

// ProjectExists reports whether <root>/.uta/project.yaml is present.
func ProjectExists(root string) bool {
	_, err := os.Stat(filepath.Join(root, ".uta", "project.yaml"))
	return err == nil
}

// Validate ensures the project struct is well-formed enough to use.
func (p *Project) Validate() error {
	if strings.TrimSpace(p.Name) == "" {
		return errors.New("project.name is required")
	}
	if p.SchemaVersion < 1 || p.SchemaVersion > CurrentSchemaVersion {
		return fmt.Errorf("unsupported project schema_version %d (this binary handles 1..%d)", p.SchemaVersion, CurrentSchemaVersion)
	}
	return nil
}
