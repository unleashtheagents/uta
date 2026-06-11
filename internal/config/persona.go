package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// Persona is a reusable system-prompt + metadata package that critic-style
// subtasks (uta audit, uta run with reflector strategy) can be paired with
// at run time. Stored at ~/.uta/personas/<id>.yaml.
type Persona struct {
	ID     string   `yaml:"id"`               // stable identifier; used as --critic and on Finding rows
	Title  string   `yaml:"title,omitempty"`  // human-readable label
	Prompt string   `yaml:"prompt"`           // appended to the critic preamble verbatim
	Worker string   `yaml:"worker,omitempty"` // optional default worker
	Tags   []string `yaml:"tags,omitempty"`

	// Force=true overrides a built-in persona with the same id.
	Force bool `yaml:"force,omitempty"`

	// Source is populated by the loader for diagnostics.
	Source string `yaml:"-"`
}

// LoadPersona reads one persona file.
func LoadPersona(path string) (*Persona, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var p Persona
	if err := yaml.Unmarshal(data, &p); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	p.Source = path
	if err := p.Validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &p, nil
}

// LoadPersonasDir scans dir for *.yaml personas. Returns parsed list +
// per-file errors (loader is fault-tolerant).
func LoadPersonasDir(dir string) ([]*Persona, []error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, []error{err}
	}
	var out []*Persona
	var errs []error
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(name, ".yaml") && !strings.HasSuffix(name, ".yml") {
			continue
		}
		p, err := LoadPersona(filepath.Join(dir, name))
		if err != nil {
			errs = append(errs, err)
			continue
		}
		out = append(out, p)
	}
	return out, errs
}

// PersonasDir returns the directory under the global home that holds
// persona YAML files, creating it on demand.
func PersonasDir(globalHome string) (string, error) {
	p := filepath.Join(globalHome, "personas")
	if err := os.MkdirAll(p, 0o755); err != nil {
		return "", err
	}
	return p, nil
}

func (p *Persona) Validate() error {
	if strings.TrimSpace(p.ID) == "" {
		return errors.New("persona.id is required")
	}
	if strings.TrimSpace(p.Prompt) == "" {
		return errors.New("persona.prompt is required")
	}
	return nil
}
