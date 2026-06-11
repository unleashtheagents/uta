package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/unleashtheagents/uta/internal/state"
)

// runThread builds a fresh thread command tree and executes it. Mirrors the
// runProject / runMode helpers used elsewhere in the suite.
func runThread(t *testing.T, args ...string) (string, string, error) {
	t.Helper()
	cmd := newThreadCmd()
	var out, errBuf bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errBuf)
	cmd.SetArgs(args)
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	err := cmd.ExecuteContext(context.Background())
	return out.String(), errBuf.String(), err
}

// setupThreadFixture isolates HOME and UTA_HOME so newApp() operates against
// an empty global home — state.json lives at $HOME/.uta/state.json. Returns
// the state directory the thread commands will read/write.
func setupThreadFixture(t *testing.T) string {
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
	return filepath.Join(home, ".uta")
}

func TestThreadNew_CreatesAndActivatesInStateJSON(t *testing.T) {
	stateDir := setupThreadFixture(t)

	out, _, err := runThread(t, "new", "dev-feature")
	if err != nil {
		t.Fatalf("thread new: %v", err)
	}
	for _, want := range []string{"created thread", "dev-feature", "now active"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}

	idx, err := state.Load(stateDir)
	if err != nil {
		t.Fatalf("state.Load: %v", err)
	}
	if len(idx.Threads) != 1 {
		t.Fatalf("threads = %d; want 1", len(idx.Threads))
	}
	thr := idx.Threads[0]
	if thr.Name != "dev-feature" {
		t.Errorf("name = %q; want dev-feature", thr.Name)
	}
	if thr.ID == "" {
		t.Errorf("thread ID should be assigned, got empty")
	}
	if idx.ActiveThread != thr.ID {
		t.Errorf("active thread = %q; want %q (new thread should be active)", idx.ActiveThread, thr.ID)
	}
	if thr.Mode != "" {
		t.Errorf("mode = %q; want empty (no --mode flag passed)", thr.Mode)
	}
}

func TestThreadNew_RecordsModeFlag(t *testing.T) {
	stateDir := setupThreadFixture(t)

	if _, _, err := runThread(t, "new", "ops-recap", "--mode", "comms"); err != nil {
		t.Fatalf("thread new: %v", err)
	}
	idx, err := state.Load(stateDir)
	if err != nil {
		t.Fatalf("state.Load: %v", err)
	}
	if len(idx.Threads) != 1 || idx.Threads[0].Mode != "comms" {
		t.Errorf("expected one thread with mode=comms; got %+v", idx.Threads)
	}
}

func TestThreadNew_RejectsDuplicateName(t *testing.T) {
	setupThreadFixture(t)

	if _, _, err := runThread(t, "new", "dup"); err != nil {
		t.Fatalf("first new: %v", err)
	}
	_, _, err := runThread(t, "new", "dup")
	if err == nil {
		t.Fatal("second new with same name: want error, got nil")
	}
	if !strings.Contains(err.Error(), "already exists") {
		t.Errorf("error = %v; want 'already exists'", err)
	}
}

func TestThreadSwitch_RejectsUnknownID(t *testing.T) {
	stateDir := setupThreadFixture(t)

	// Seed one real thread so the only failure mode is the bogus arg.
	if _, _, err := runThread(t, "new", "real"); err != nil {
		t.Fatalf("thread new: %v", err)
	}
	pre, err := state.Load(stateDir)
	if err != nil {
		t.Fatalf("state.Load: %v", err)
	}
	wantActive := pre.ActiveThread

	_, _, err = runThread(t, "switch", "ghost-id")
	if err == nil {
		t.Fatal("switch to unknown id: want error, got nil")
	}
	if !strings.Contains(err.Error(), "no thread matching") {
		t.Errorf("error = %v; want 'no thread matching'", err)
	}

	// A rejected switch must not mutate the active pointer.
	post, err := state.Load(stateDir)
	if err != nil {
		t.Fatalf("state.Load post: %v", err)
	}
	if post.ActiveThread != wantActive {
		t.Errorf("active thread mutated by rejected switch: got %q, want %q",
			post.ActiveThread, wantActive)
	}
}

func TestThreadSwitch_AcceptsNameAndUpdatesActive(t *testing.T) {
	stateDir := setupThreadFixture(t)

	if _, _, err := runThread(t, "new", "first"); err != nil {
		t.Fatalf("new first: %v", err)
	}
	if _, _, err := runThread(t, "new", "second"); err != nil {
		t.Fatalf("new second: %v", err)
	}
	// second is currently active (new auto-activates). Switch back to first.
	out, _, err := runThread(t, "switch", "first")
	if err != nil {
		t.Fatalf("switch first: %v", err)
	}
	if !strings.Contains(out, "switched to") || !strings.Contains(out, "first") {
		t.Errorf("output missing switch confirmation:\n%s", out)
	}

	idx, err := state.Load(stateDir)
	if err != nil {
		t.Fatalf("state.Load: %v", err)
	}
	active := idx.Active()
	if active == nil || active.Name != "first" {
		t.Errorf("active thread = %+v; want name=first", active)
	}
}

func TestThreadList_EmptyMessage(t *testing.T) {
	setupThreadFixture(t)

	out, _, err := runThread(t, "list")
	if err != nil {
		t.Fatalf("thread list: %v", err)
	}
	if !strings.Contains(out, "no threads yet") {
		t.Errorf("empty list should mention 'no threads yet':\n%s", out)
	}
}

func TestThreadList_TableMarksActive(t *testing.T) {
	setupThreadFixture(t)

	if _, _, err := runThread(t, "new", "alpha"); err != nil {
		t.Fatalf("new alpha: %v", err)
	}
	if _, _, err := runThread(t, "new", "beta"); err != nil {
		t.Fatalf("new beta: %v", err)
	}

	out, _, err := runThread(t, "list")
	if err != nil {
		t.Fatalf("thread list: %v", err)
	}
	for _, want := range []string{"ACTIVE", "NAME", "MODE", "LAST USED", "alpha", "beta"} {
		if !strings.Contains(out, want) {
			t.Errorf("table missing %q:\n%s", want, out)
		}
	}
	// beta was created last so it should be the active one (marked with *).
	if !strings.Contains(out, "* ") && !strings.Contains(out, "*\t") {
		t.Errorf("active marker '*' missing from table:\n%s", out)
	}
}

func TestThreadList_JSONSchema(t *testing.T) {
	setupThreadFixture(t)

	if _, _, err := runThread(t, "new", "json-thread", "--mode", "dev"); err != nil {
		t.Fatalf("new: %v", err)
	}

	out, _, err := runThread(t, "list", "--json")
	if err != nil {
		t.Fatalf("thread list --json: %v", err)
	}

	var idx struct {
		Version      int    `json:"version"`
		ActiveThread string `json:"active_thread"`
		Threads      []struct {
			ID              string `json:"id"`
			Name            string `json:"name"`
			Mode            string `json:"mode"`
			ActiveSessionID string `json:"active_session_id"`
			CreatedAt       string `json:"created_at"`
			LastUsedAt      string `json:"last_used_at"`
		} `json:"threads"`
	}
	if err := json.Unmarshal([]byte(out), &idx); err != nil {
		t.Fatalf("decode json: %v\nraw=%s", err, out)
	}
	if idx.Version == 0 {
		t.Errorf("version should be set; got 0")
	}
	if len(idx.Threads) != 1 {
		t.Fatalf("threads = %d; want 1", len(idx.Threads))
	}
	thr := idx.Threads[0]
	if thr.Name != "json-thread" {
		t.Errorf("thread name = %q; want json-thread", thr.Name)
	}
	if thr.Mode != "dev" {
		t.Errorf("thread mode = %q; want dev", thr.Mode)
	}
	if thr.ID == "" || idx.ActiveThread != thr.ID {
		t.Errorf("active_thread = %q; want %q", idx.ActiveThread, thr.ID)
	}
	if thr.CreatedAt == "" || thr.LastUsedAt == "" {
		t.Errorf("timestamps should be populated: created=%q last_used=%q",
			thr.CreatedAt, thr.LastUsedAt)
	}
}

func TestThreadList_JSONFlagDoesNotMutateState(t *testing.T) {
	stateDir := setupThreadFixture(t)

	if _, _, err := runThread(t, "new", "stable"); err != nil {
		t.Fatalf("new: %v", err)
	}
	before, err := os.ReadFile(state.Path(stateDir))
	if err != nil {
		t.Fatalf("read state pre: %v", err)
	}

	if _, _, err := runThread(t, "list", "--json"); err != nil {
		t.Fatalf("list --json: %v", err)
	}
	if _, _, err := runThread(t, "list"); err != nil {
		t.Fatalf("list: %v", err)
	}

	after, err := os.ReadFile(state.Path(stateDir))
	if err != nil {
		t.Fatalf("read state post: %v", err)
	}
	if !bytes.Equal(before, after) {
		t.Errorf("list mutated state.json:\nbefore=%s\nafter=%s", before, after)
	}
}

func TestThreadCurrent_ErrorsWhenNoActive(t *testing.T) {
	setupThreadFixture(t)

	_, _, err := runThread(t, "current")
	if err == nil {
		t.Fatal("current with no threads: want error, got nil")
	}
	if !strings.Contains(err.Error(), "no active thread") {
		t.Errorf("error = %v; want 'no active thread'", err)
	}
}

func TestThreadCurrent_JSONSchema(t *testing.T) {
	setupThreadFixture(t)

	if _, _, err := runThread(t, "new", "focus", "--mode", "audit"); err != nil {
		t.Fatalf("new: %v", err)
	}

	out, _, err := runThread(t, "current", "--json")
	if err != nil {
		t.Fatalf("current --json: %v", err)
	}
	var thr struct {
		ID         string `json:"id"`
		Name       string `json:"name"`
		Mode       string `json:"mode"`
		CreatedAt  string `json:"created_at"`
		LastUsedAt string `json:"last_used_at"`
	}
	if err := json.Unmarshal([]byte(out), &thr); err != nil {
		t.Fatalf("decode json: %v\nraw=%s", err, out)
	}
	if thr.Name != "focus" {
		t.Errorf("name = %q; want focus", thr.Name)
	}
	if thr.Mode != "audit" {
		t.Errorf("mode = %q; want audit", thr.Mode)
	}
	if thr.ID == "" {
		t.Errorf("id should be populated, got empty")
	}
}
