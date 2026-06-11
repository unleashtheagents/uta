package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestProviderDescriptor_Validate(t *testing.T) {
	base := func() ProviderDescriptor {
		return ProviderDescriptor{
			Name:   "myprov",
			Binary: "/usr/bin/myprov",
			Invocation: ProviderDescriptorInvocation{
				Argv: []string{"--prompt", "{{prompt}}"},
			},
		}
	}

	cases := []struct {
		name      string
		mutate    func(*ProviderDescriptor)
		wantErr   bool
		errSubstr string
	}{
		{
			name:    "ok minimal",
			mutate:  func(d *ProviderDescriptor) {},
			wantErr: false,
		},
		{
			name:      "missing name",
			mutate:    func(d *ProviderDescriptor) { d.Name = "" },
			wantErr:   true,
			errSubstr: "name",
		},
		{
			name:      "whitespace name",
			mutate:    func(d *ProviderDescriptor) { d.Name = "   " },
			wantErr:   true,
			errSubstr: "name",
		},
		{
			name:      "missing binary",
			mutate:    func(d *ProviderDescriptor) { d.Binary = "" },
			wantErr:   true,
			errSubstr: "binary",
		},
		{
			name:      "whitespace binary",
			mutate:    func(d *ProviderDescriptor) { d.Binary = "\t " },
			wantErr:   true,
			errSubstr: "binary",
		},
		{
			name:      "missing argv",
			mutate:    func(d *ProviderDescriptor) { d.Invocation.Argv = nil },
			wantErr:   true,
			errSubstr: "argv",
		},
		{
			name:      "empty argv slice",
			mutate:    func(d *ProviderDescriptor) { d.Invocation.Argv = []string{} },
			wantErr:   true,
			errSubstr: "argv",
		},
		{
			name:    "output.format empty is allowed",
			mutate:  func(d *ProviderDescriptor) { d.Output.Format = "" },
			wantErr: false,
		},
		{
			name:    "output.format stream-json allowed",
			mutate:  func(d *ProviderDescriptor) { d.Output.Format = "stream-json" },
			wantErr: false,
		},
		{
			name:    "output.format text allowed",
			mutate:  func(d *ProviderDescriptor) { d.Output.Format = "text" },
			wantErr: false,
		},
		{
			name:      "output.format unknown rejected",
			mutate:    func(d *ProviderDescriptor) { d.Output.Format = "yaml" },
			wantErr:   true,
			errSubstr: "output.format",
		},
		{
			name:      "output.format json (not aliased to stream-json) rejected",
			mutate:    func(d *ProviderDescriptor) { d.Output.Format = "json" },
			wantErr:   true,
			errSubstr: "output.format",
		},
		{
			name:    "version_regex empty is allowed (default applied later)",
			mutate:  func(d *ProviderDescriptor) { d.Detect.VersionRegex = "" },
			wantErr: false,
		},
		{
			name:    "version_regex valid",
			mutate:  func(d *ProviderDescriptor) { d.Detect.VersionRegex = `v(\d+\.\d+)` },
			wantErr: false,
		},
		{
			name:      "version_regex invalid",
			mutate:    func(d *ProviderDescriptor) { d.Detect.VersionRegex = `v(\d+` },
			wantErr:   true,
			errSubstr: "version_regex",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := base()
			tc.mutate(&d)
			err := d.Validate()
			if tc.wantErr {
				if err == nil {
					t.Fatalf("want error, got nil")
				}
				if tc.errSubstr != "" && !strings.Contains(err.Error(), tc.errSubstr) {
					t.Errorf("error %q does not mention %q", err.Error(), tc.errSubstr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

func TestProviderDescriptor_applyDefaults(t *testing.T) {
	t.Run("fills empty fields", func(t *testing.T) {
		d := &ProviderDescriptor{
			Name:   "x",
			Binary: "x",
			Invocation: ProviderDescriptorInvocation{
				Argv: []string{"{{prompt}}"},
			},
		}
		d.applyDefaults()

		if got, want := d.Detect.Args, []string{"--version"}; !equalSlices(got, want) {
			t.Errorf("Detect.Args: got %v, want %v", got, want)
		}
		if d.Detect.VersionRegex != `(\d+\.\d+(?:\.\d+)?)` {
			t.Errorf("Detect.VersionRegex: got %q", d.Detect.VersionRegex)
		}
		if d.Output.Format != "text" {
			t.Errorf("Output.Format: got %q, want %q", d.Output.Format, "text")
		}
		if d.Output.TextField != "text" {
			t.Errorf("Output.TextField: got %q, want %q", d.Output.TextField, "text")
		}
	})

	t.Run("preserves user-supplied values", func(t *testing.T) {
		d := &ProviderDescriptor{
			Detect: ProviderDescriptorDetect{
				Args:         []string{"version"},
				VersionRegex: `release-(\d+)`,
			},
			Output: ProviderDescriptorOutput{
				Format:    "stream-json",
				TextField: "content",
			},
		}
		d.applyDefaults()

		if got, want := d.Detect.Args, []string{"version"}; !equalSlices(got, want) {
			t.Errorf("Detect.Args overwritten: got %v, want %v", got, want)
		}
		if d.Detect.VersionRegex != `release-(\d+)` {
			t.Errorf("Detect.VersionRegex overwritten: got %q", d.Detect.VersionRegex)
		}
		if d.Output.Format != "stream-json" {
			t.Errorf("Output.Format overwritten: got %q", d.Output.Format)
		}
		if d.Output.TextField != "content" {
			t.Errorf("Output.TextField overwritten: got %q", d.Output.TextField)
		}
	})

	t.Run("empty Detect.Args slice gets default", func(t *testing.T) {
		// Both nil and len==0 should be replaced by the default.
		d := &ProviderDescriptor{
			Detect: ProviderDescriptorDetect{Args: []string{}},
		}
		d.applyDefaults()
		if got, want := d.Detect.Args, []string{"--version"}; !equalSlices(got, want) {
			t.Errorf("Detect.Args: got %v, want %v", got, want)
		}
	})
}

func TestLoadProviderDescriptor_AppliesDefaultsAfterValidate(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "p.yaml")
	yaml := `name: cool
binary: /usr/bin/cool
invocation:
  argv: ["--prompt", "{{prompt}}"]
`
	if err := os.WriteFile(path, []byte(yaml), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	d, err := LoadProviderDescriptor(path)
	if err != nil {
		t.Fatalf("LoadProviderDescriptor: %v", err)
	}
	if d.Source != path {
		t.Errorf("Source: got %q, want %q", d.Source, path)
	}
	if d.Output.Format != "text" {
		t.Errorf("Output.Format default not applied: got %q", d.Output.Format)
	}
	if d.Output.TextField != "text" {
		t.Errorf("Output.TextField default not applied: got %q", d.Output.TextField)
	}
	if d.Detect.VersionRegex == "" {
		t.Errorf("Detect.VersionRegex default not applied")
	}
	if len(d.Detect.Args) == 0 {
		t.Errorf("Detect.Args default not applied")
	}
}

func TestLoadProviderDescriptor_RejectsBadOutputFormat(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "p.yaml")
	yaml := `name: cool
binary: /usr/bin/cool
invocation:
  argv: ["{{prompt}}"]
output:
  format: ndjson
`
	if err := os.WriteFile(path, []byte(yaml), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	_, err := LoadProviderDescriptor(path)
	if err == nil {
		t.Fatalf("want error for invalid output.format, got nil")
	}
	if !strings.Contains(err.Error(), "output.format") {
		t.Errorf("error should mention output.format: %v", err)
	}
}

func TestLoadProviderDescriptor_RejectsUnknownYAMLFields(t *testing.T) {
	// KnownFields(true) is set on the decoder — typos in field names must fail.
	dir := t.TempDir()
	path := filepath.Join(dir, "p.yaml")
	yaml := `name: cool
binary: /usr/bin/cool
bogus_field: oops
invocation:
  argv: ["{{prompt}}"]
`
	if err := os.WriteFile(path, []byte(yaml), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	_, err := LoadProviderDescriptor(path)
	if err == nil {
		t.Fatalf("want parse error for unknown field, got nil")
	}
}

func TestLoadProviderDescriptor_FileMissing(t *testing.T) {
	_, err := LoadProviderDescriptor(filepath.Join(t.TempDir(), "nope.yaml"))
	if err == nil {
		t.Fatalf("want error for missing file, got nil")
	}
	if !errors.Is(err, os.ErrNotExist) {
		t.Errorf("want os.ErrNotExist, got %v", err)
	}
}

func TestLoadProvidersDir_MissingDirIsNotAnError(t *testing.T) {
	out, errs := LoadProvidersDir(filepath.Join(t.TempDir(), "absent"))
	if len(out) != 0 || len(errs) != 0 {
		t.Errorf("missing dir: got out=%v errs=%v, want empty/empty", out, errs)
	}
}

func TestLoadProvidersDir_SkipsNonYAMLAndCollectsErrors(t *testing.T) {
	dir := t.TempDir()
	// Valid descriptor.
	good := `name: good
binary: /usr/bin/good
invocation:
  argv: ["{{prompt}}"]
`
	if err := os.WriteFile(filepath.Join(dir, "good.yaml"), []byte(good), 0o644); err != nil {
		t.Fatalf("write good: %v", err)
	}
	// Valid .yml extension also accepted.
	if err := os.WriteFile(filepath.Join(dir, "alt.yml"), []byte(good), 0o644); err != nil {
		t.Fatalf("write alt: %v", err)
	}
	// Wrong extension — ignored.
	if err := os.WriteFile(filepath.Join(dir, "ignore.txt"), []byte("not yaml"), 0o644); err != nil {
		t.Fatalf("write ignored: %v", err)
	}
	// Subdirectory — ignored (only loose files are scanned).
	if err := os.MkdirAll(filepath.Join(dir, "subdir"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// Malformed descriptor — error collected, others still loaded.
	bad := "name: bad\nbinary: /usr/bin/bad\n: not yaml\n"
	if err := os.WriteFile(filepath.Join(dir, "bad.yaml"), []byte(bad), 0o644); err != nil {
		t.Fatalf("write bad: %v", err)
	}

	out, errs := LoadProvidersDir(dir)
	if len(out) != 2 {
		t.Errorf("loaded descriptors: got %d, want 2", len(out))
	}
	if len(errs) != 1 {
		t.Errorf("collected errors: got %d, want 1 (%v)", len(errs), errs)
	}
	names := map[string]bool{}
	for _, d := range out {
		names[d.Name] = true
	}
	if !names["good"] {
		t.Errorf("expected 'good' descriptor in output, got %v", names)
	}
}

func equalSlices(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
