package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/unleashtheagents/uta/internal/profile"
)

func TestSourceLabel(t *testing.T) {
	cases := []struct {
		name        string
		src         string
		globalHome  string
		projectRoot string
		want        string
	}{
		{
			name: "empty-stays-empty",
			src:  "",
			want: "",
		},
		{
			name: "embedded-passes-through",
			src:  profile.EmbeddedSource,
			want: profile.EmbeddedSource,
		},
		{
			name:        "project-prefix-stripped",
			src:         "/work/proj/.uta/profiles/dev.yaml",
			projectRoot: "/work/proj",
			want:        "project:.uta/profiles/dev.yaml",
		},
		{
			name:       "global-prefix-stripped",
			src:        "/home/user/.uta/profiles/audit.yaml",
			globalHome: "/home/user/.uta",
			want:       "global:profiles/audit.yaml",
		},
		{
			name:        "project-wins-over-global-when-both-could-match",
			src:         "/work/proj/.uta/profiles/dev.yaml",
			projectRoot: "/work/proj",
			globalHome:  "/work",
			want:        "project:.uta/profiles/dev.yaml",
		},
		{
			name:        "unrelated-path-falls-through",
			src:         "/elsewhere/profile.yaml",
			projectRoot: "/work/proj",
			globalHome:  "/home/user/.uta",
			want:        "/elsewhere/profile.yaml",
		},
		{
			name: "no-roots-supplied-falls-through",
			src:  "/some/where/profile.yaml",
			want: "/some/where/profile.yaml",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := sourceLabel(tc.src, tc.globalHome, tc.projectRoot)
			if got != tc.want {
				t.Errorf("sourceLabel(%q, %q, %q) = %q; want %q",
					tc.src, tc.globalHome, tc.projectRoot, got, tc.want)
			}
		})
	}
}

func TestFirstLine(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"", ""},
		{"only-line", "only-line"},
		{"one\ntwo\nthree", "one"},
		{"  \n\nactual content\nnext", "actual content"},
		{"   \n\t\n", ""},
	}
	for _, tc := range cases {
		got := firstLine(tc.in)
		if got != tc.want {
			t.Errorf("firstLine(%q) = %q; want %q", tc.in, got, tc.want)
		}
	}
}

func TestStripPrefix(t *testing.T) {
	if got, ok := stripPrefix("/a/b/c", "/a/"); !ok || got != "b/c" {
		t.Errorf("stripPrefix match: got (%q,%v); want (b/c,true)", got, ok)
	}
	if got, ok := stripPrefix("/x/y", "/a/"); ok || got != "" {
		t.Errorf("stripPrefix miss: got (%q,%v); want (\"\",false)", got, ok)
	}
}

// newModeFlagCmd builds a minimal cobra command exposing the same --mode
// string flag the root command does. resolveActiveMode reads the flag by
// name; the rest of the cobra wiring is irrelevant for this unit test.
func newModeFlagCmd(t *testing.T, modeFlag string) *cobra.Command {
	t.Helper()
	cmd := &cobra.Command{Use: "fake"}
	cmd.Flags().String("mode", modeFlag, "")
	return cmd
}

func TestResolveActiveMode_EmptyFlagReturnsNilNil(t *testing.T) {
	cmd := newModeFlagCmd(t, "")
	cmd.SetErr(&bytes.Buffer{})
	app := &App{GlobalHome: t.TempDir()}

	p, err := resolveActiveMode(cmd, app)
	if err != nil {
		t.Fatalf("resolveActiveMode: %v", err)
	}
	if p != nil {
		t.Errorf("empty --mode should yield nil profile, got %+v", p)
	}
}

func TestResolveActiveMode_WhitespaceFlagTreatedAsUnset(t *testing.T) {
	cmd := newModeFlagCmd(t, "   ")
	cmd.SetErr(&bytes.Buffer{})
	app := &App{GlobalHome: t.TempDir()}

	p, err := resolveActiveMode(cmd, app)
	if err != nil {
		t.Fatalf("resolveActiveMode: %v", err)
	}
	if p != nil {
		t.Errorf("whitespace-only --mode should be unset, got %+v", p)
	}
}

func TestResolveActiveMode_DefaultProfileResolvesViaEmbedded(t *testing.T) {
	cmd := newModeFlagCmd(t, profile.DefaultProfileName)
	var stderr bytes.Buffer
	cmd.SetErr(&stderr)
	// A non-existent global home is fine — LoadAll tolerates it and the
	// embedded default is always present.
	app := &App{GlobalHome: filepath.Join(t.TempDir(), "no-such-home")}

	p, err := resolveActiveMode(cmd, app)
	if err != nil {
		t.Fatalf("resolveActiveMode: %v (stderr=%q)", err, stderr.String())
	}
	if p == nil {
		t.Fatal("expected embedded default to resolve, got nil")
	}
	if p.Name != profile.DefaultProfileName {
		t.Errorf("resolved name = %q; want %q", p.Name, profile.DefaultProfileName)
	}
	if p.Source != profile.EmbeddedSource {
		t.Errorf("resolved source = %q; want %q", p.Source, profile.EmbeddedSource)
	}
}

func TestResolveActiveMode_UnknownNameReturnsExit2(t *testing.T) {
	cmd := newModeFlagCmd(t, "no-such-mode")
	var stderr bytes.Buffer
	cmd.SetErr(&stderr)
	app := &App{GlobalHome: t.TempDir()}

	p, err := resolveActiveMode(cmd, app)
	if err == nil {
		t.Fatalf("resolveActiveMode unknown mode: want error, got nil profile=%+v", p)
	}
	if p != nil {
		t.Errorf("unknown mode should return nil profile, got %+v", p)
	}
	var exitErr *exitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("error is not *exitError: %T %v", err, err)
	}
	if exitErr.code != 2 {
		t.Errorf("exit code = %d; want 2", exitErr.code)
	}
	if !strings.Contains(stderr.String(), "no profile named") {
		t.Errorf("stderr should explain the missing name: %q", stderr.String())
	}
	if !strings.Contains(stderr.String(), "uta mode list") {
		t.Errorf("stderr should hint at `uta mode list`: %q", stderr.String())
	}
}

// runMode builds a fresh mode command tree and executes it. Matches the
// pattern used in project_test.go / ctx_test.go so the test suite stays
// uniform in how it pokes cobra trees.
func runMode(t *testing.T, args ...string) (string, string, error) {
	t.Helper()
	cmd := newModeCmd()
	var out, errBuf bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errBuf)
	cmd.SetArgs(args)
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	err := cmd.ExecuteContext(context.Background())
	return out.String(), errBuf.String(), err
}

// setupModeFixture isolates HOME and UTA_HOME so newApp() builds an App
// against a throwaway global home with no recorded sessions — meaning
// Store.ModeStats returns an empty map and `mode list` must render the
// embedded default with zeroed stats.
func setupModeFixture(t *testing.T) string {
	t.Helper()
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
	return dir
}

func TestModeList_TableRendersZeroStats(t *testing.T) {
	setupModeFixture(t)

	out, _, err := runMode(t, "list")
	if err != nil {
		t.Fatalf("mode list: %v", err)
	}
	for _, want := range []string{"NAME", "PERSONAS", "SESSIONS", "LAST USED", "SOURCE"} {
		if !strings.Contains(out, want) {
			t.Errorf("table header missing %q:\n%s", want, out)
		}
	}
	// The embedded default profile must show up with a zero session
	// count and a "-" placeholder for last-used, proving the
	// missing-stats path renders gracefully.
	if !strings.Contains(out, profile.DefaultProfileName) {
		t.Errorf("output missing default profile:\n%s", out)
	}
	if !strings.Contains(out, profile.EmbeddedSource) {
		t.Errorf("output missing embedded source label:\n%s", out)
	}
	if !strings.Contains(out, "-") {
		t.Errorf("output missing '-' placeholder for last-used:\n%s", out)
	}
}

func TestModeList_JSONRendersZeroStats(t *testing.T) {
	setupModeFixture(t)

	out, _, err := runMode(t, "list", "--json")
	if err != nil {
		t.Fatalf("mode list --json: %v", err)
	}
	var rows []struct {
		Name                string  `json:"name"`
		Description         string  `json:"description,omitempty"`
		Source              string  `json:"source"`
		Personas            int     `json:"personas"`
		MCPServers          int     `json:"mcp_servers"`
		AllowedTools        int     `json:"allowed_tools"`
		DeniedTools         int     `json:"denied_tools"`
		LastUsedAt          string  `json:"last_used_at,omitempty"`
		TotalSessions       int     `json:"total_sessions"`
		TotalSubtaskSeconds float64 `json:"total_subtask_seconds"`
	}
	if err := json.Unmarshal([]byte(out), &rows); err != nil {
		t.Fatalf("decode json: %v\nraw=%s", err, out)
	}
	if len(rows) == 0 {
		t.Fatalf("expected at least one profile row, got 0:\n%s", out)
	}
	var seenDefault bool
	for _, r := range rows {
		if r.Name == profile.DefaultProfileName {
			seenDefault = true
			if r.Source != profile.EmbeddedSource {
				t.Errorf("default profile source = %q; want %q", r.Source, profile.EmbeddedSource)
			}
			if r.TotalSessions != 0 {
				t.Errorf("default TotalSessions = %d; want 0 in fresh fixture", r.TotalSessions)
			}
			if r.TotalSubtaskSeconds != 0 {
				t.Errorf("default TotalSubtaskSeconds = %v; want 0", r.TotalSubtaskSeconds)
			}
			// LastUsedAt is omitempty — for a never-used mode the JSON
			// encoder drops the field entirely, leaving the zero value.
			if r.LastUsedAt != "" {
				t.Errorf("default LastUsedAt = %q; want empty when never used", r.LastUsedAt)
			}
		}
	}
	if !seenDefault {
		t.Errorf("default profile not present in JSON output:\n%s", out)
	}
}
