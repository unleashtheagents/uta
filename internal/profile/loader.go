package profile

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// GlobalProfilesDir returns the path holding user-wide YAML profiles
// under the given uta home. Does NOT create the directory; the loader
// tolerates a missing dir.
func GlobalProfilesDir(globalHome string) string {
	return filepath.Join(globalHome, "profiles")
}

// ProjectProfilesDir returns the project-local YAML profile dir nested
// under <projectRoot>/.uta/profiles. Returns "" when projectRoot is empty.
func ProjectProfilesDir(projectRoot string) string {
	if projectRoot == "" {
		return ""
	}
	return filepath.Join(projectRoot, ".uta", "profiles")
}

// Load parses a single profile from disk, applying KnownFields(true) so
// unknown YAML keys fail loud.
func Load(path string) (*MissionProfile, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	p, err := parseProfile(data, path)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return p, nil
}

// parseProfile is the shared YAML-bytes -> MissionProfile path used by
// both the on-disk loader and the embedded-default constructor.
func parseProfile(data []byte, source string) (*MissionProfile, error) {
	var p MissionProfile
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&p); err != nil {
		return nil, fmt.Errorf("parse: %w", err)
	}
	p.Source = source
	if err := p.Validate(); err != nil {
		return nil, err
	}
	return &p, nil
}

// LoadDir scans dir for *.yaml / *.yml profile files. Returns the parsed
// list and any per-file errors. A missing dir is not an error (returns
// nil, nil) so callers can treat the global and project dirs uniformly.
//
// The output is sorted by profile name for stable diagnostics.
func LoadDir(dir string) ([]*MissionProfile, []error) {
	if dir == "" {
		return nil, nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, []error{err}
	}
	var out []*MissionProfile
	var errs []error
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(name, ".yaml") && !strings.HasSuffix(name, ".yml") {
			continue
		}
		full := filepath.Join(dir, name)
		p, err := Load(full)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, errs
}

// LoadAll resolves the full set of available profiles by layering, from
// lowest to highest precedence:
//
//  1. The embedded "default" profile.
//  2. YAML profiles in <globalHome>/profiles/.
//  3. YAML profiles in <projectRoot>/.uta/profiles/ (project-local wins).
//
// Profiles are keyed by Name (case-sensitive). A later layer with the
// same Name replaces the earlier one wholesale; field-level merging is
// intentionally not done — a user who copies a profile to override it
// should be explicit about every field.
//
// The returned slice is sorted by Name. Per-file parse/validate errors
// are returned alongside the (partial) profile list rather than failing
// the whole call, matching the fault-tolerant loader pattern used for
// provider descriptors.
func LoadAll(globalHome, projectRoot string) ([]*MissionProfile, []error) {
	byName := map[string]*MissionProfile{
		DefaultProfileName: EmbeddedDefault(),
	}

	var errs []error

	globalDir := ""
	if globalHome != "" {
		globalDir = GlobalProfilesDir(globalHome)
	}
	globals, gErrs := LoadDir(globalDir)
	errs = append(errs, gErrs...)
	for _, p := range globals {
		byName[p.Name] = p
	}

	projectDir := ProjectProfilesDir(projectRoot)
	projects, pErrs := LoadDir(projectDir)
	errs = append(errs, pErrs...)
	for _, p := range projects {
		byName[p.Name] = p
	}

	out := make([]*MissionProfile, 0, len(byName))
	for _, p := range byName {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, errs
}

// Find returns the profile with the given name from a previously-loaded
// list. Returns an error wrapped with the available names when no match
// is found, so CLI callers can produce a clean diagnostic.
func Find(profiles []*MissionProfile, name string) (*MissionProfile, error) {
	for _, p := range profiles {
		if p.Name == name {
			return p, nil
		}
	}
	available := make([]string, 0, len(profiles))
	for _, p := range profiles {
		available = append(available, p.Name)
	}
	sort.Strings(available)
	return nil, fmt.Errorf("no profile named %q (available: %s)", name, strings.Join(available, ", "))
}
