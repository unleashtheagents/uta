package config

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

// ProviderDescriptor is the on-disk shape of a YAML file under
// ~/.uta/providers/. It lets users add a new agent CLI as a provider
// without recompiling uta.
type ProviderDescriptor struct {
	Name         string                       `yaml:"name"`
	Binary       string                       `yaml:"binary"`
	Notes        string                       `yaml:"notes"`
	Detect       ProviderDescriptorDetect     `yaml:"detect"`
	Invocation   ProviderDescriptorInvocation `yaml:"invocation"`
	Output       ProviderDescriptorOutput     `yaml:"output"`
	Capabilities []string                     `yaml:"capabilities"`

	// Force=true lets a user override a built-in with the same name.
	Force bool `yaml:"force"`

	// Source path is populated by the loader for diagnostics.
	Source string `yaml:"-"`
}

type ProviderDescriptorDetect struct {
	Args         []string `yaml:"args"`          // args passed to the binary for version probe; default ["--version"]
	VersionRegex string   `yaml:"version_regex"` // first capture group is the version; default '(\d+\.\d+(?:\.\d+)?)'
}

type ProviderDescriptorInvocation struct {
	Argv       []string `yaml:"argv"`        // template strings with {{prompt}}, {{output_format}}, {{workdir}}
	Stdin      string   `yaml:"stdin"`       // optional; if non-empty, prompt is fed via stdin (template)
	ResumeArgv []string `yaml:"resume_argv"` // optional; presence implies the provider supports resume
	// MCPConfigArgv is appended to the final argv only when the engine has
	// materialized an MCP config file for this run (opts.MCPConfigPath
	// non-empty). Use the {{mcp_config}} template var to interpolate the
	// path — e.g. ["--mcp-config", "{{mcp_config}}"]. Empty means the
	// provider has no per-invocation MCP flag, so the bridge's config path
	// is dropped on the floor for this provider (which is the right call
	// when the underlying CLI ingests MCP servers from its own user
	// config rather than from a CLI flag).
	MCPConfigArgv []string          `yaml:"mcp_config_argv"`
	Env           map[string]string `yaml:"env"`
}

type ProviderDescriptorOutput struct {
	// Format is "stream-json" or "text". stream-json: each stdout line is a
	// JSON object; uta extracts SessionIDField and FinalTextField when found.
	// text: stdout is captured verbatim as the final answer.
	Format         string            `yaml:"format"`
	SessionIDField string            `yaml:"session_id_field"`
	FinalTextField string            `yaml:"final_text_field"`
	TextField      string            `yaml:"text_field"` // field within message blocks that holds assistant text; default "text"
	EventDispatch  map[string]string `yaml:"event_dispatch"`
}

// LoadProvidersDir scans dir for *.yaml descriptors and returns the parsed list.
// Files that fail to parse are skipped but the error is collected so callers
// can surface it (uta doctor / uta providers --refresh).
func LoadProvidersDir(dir string) ([]*ProviderDescriptor, []error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, []error{err}
	}
	var out []*ProviderDescriptor
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
		d, err := LoadProviderDescriptor(full)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", full, err))
			continue
		}
		out = append(out, d)
	}
	return out, errs
}

func LoadProviderDescriptor(path string) (*ProviderDescriptor, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var d ProviderDescriptor
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&d); err != nil {
		return nil, fmt.Errorf("parse: %w", err)
	}
	d.Source = path
	if err := d.Validate(); err != nil {
		return nil, err
	}
	d.applyDefaults()
	return &d, nil
}

func (d *ProviderDescriptor) Validate() error {
	if strings.TrimSpace(d.Name) == "" {
		return errors.New("name is required")
	}
	if strings.TrimSpace(d.Binary) == "" {
		return errors.New("binary is required")
	}
	if len(d.Invocation.Argv) == 0 {
		return errors.New("invocation.argv is required")
	}
	switch d.Output.Format {
	case "", "stream-json", "text":
	default:
		return fmt.Errorf("output.format must be 'stream-json' or 'text' (got %q)", d.Output.Format)
	}
	if d.Detect.VersionRegex != "" {
		if _, err := regexp.Compile(d.Detect.VersionRegex); err != nil {
			return fmt.Errorf("detect.version_regex invalid: %w", err)
		}
	}
	return nil
}

func (d *ProviderDescriptor) applyDefaults() {
	if len(d.Detect.Args) == 0 {
		d.Detect.Args = []string{"--version"}
	}
	if d.Detect.VersionRegex == "" {
		d.Detect.VersionRegex = `(\d+\.\d+(?:\.\d+)?)`
	}
	if d.Output.Format == "" {
		d.Output.Format = "text"
	}
	if d.Output.TextField == "" {
		d.Output.TextField = "text"
	}
}
