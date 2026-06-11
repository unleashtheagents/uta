package engine

import (
	"context"
	"runtime"
	"strings"
	"testing"
)

func TestMergeEnv_OverridesAndPreservesOrder(t *testing.T) {
	base := []string{"PATH=/usr/bin", "FOO=base", "BAR=keep"}
	got := mergeEnv(base, map[string]string{"FOO": "override", "NEW": "added"})

	// PATH and BAR (not overridden) must appear in their original order
	// before any override entries.
	if got[0] != "PATH=/usr/bin" {
		t.Errorf("expected PATH first, got %q", got[0])
	}
	if got[1] != "BAR=keep" {
		t.Errorf("expected BAR second (FOO removed), got %q", got[1])
	}

	// Overrides should be appended sorted by key for determinism.
	tail := got[2:]
	if len(tail) != 2 || tail[0] != "FOO=override" || tail[1] != "NEW=added" {
		t.Errorf("expected sorted override tail [FOO=override NEW=added], got %v", tail)
	}
}

func TestMergeEnv_EmptyOverridesReturnsBase(t *testing.T) {
	base := []string{"A=1", "B=2"}
	got := mergeEnv(base, nil)
	if len(got) != 2 || got[0] != "A=1" || got[1] != "B=2" {
		t.Errorf("expected base preserved when overrides empty, got %v", got)
	}
}

func TestRunTool_AppliesEnvOverride(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell-based env smoke test is POSIX-only")
	}
	spec := ToolSpec{
		ID:        "envprobe",
		Cmd:       "printf '%s' \"$UTA_TOOL_TOKEN\"",
		ShellMode: true,
		Adapter:   "raw",
		Env:       map[string]string{"UTA_TOOL_TOKEN": "sekret"},
	}
	res := runTool(context.Background(), spec)
	if res.Err != nil {
		t.Fatalf("unexpected error: %v", res.Err)
	}
	if strings.TrimSpace(res.Stdout) != "sekret" {
		t.Errorf("expected env override to reach the tool, got stdout=%q stderr=%q", res.Stdout, res.Stderr)
	}
}
