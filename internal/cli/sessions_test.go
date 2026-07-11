package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/unleashtheagents/uta/internal/paths"
	"github.com/unleashtheagents/uta/internal/store"
)

func TestShortID(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"empty", "", ""},
		{"shorter-than-8", "abc", "abc"},
		{"exactly-8", "abcdefgh", "abcdefgh"},
		{"longer-than-8", "0123456789abcdef", "01234567"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := shortID(tc.in); got != tc.want {
				t.Errorf("shortID(%q) = %q; want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestSingleLine_FlattensNewlinesAndCarriageReturns(t *testing.T) {
	in := "line1\nline2\rline3\r\nline4"
	got := singleLine(in)
	// Each \n or \r becomes a single space; \r\n becomes two spaces because
	// singleLine substitutes per-rune, not per-sequence.
	want := "line1 line2 line3  line4"
	if got != want {
		t.Errorf("singleLine(%q) = %q; want %q", in, got, want)
	}
	if strings.ContainsAny(got, "\n\r") {
		t.Errorf("singleLine left newline/cr in output: %q", got)
	}
}

func TestTruncateLine(t *testing.T) {
	cases := []struct {
		name string
		in   string
		max  int
		want string
	}{
		{"shorter-than-max", "hello", 60, "hello"},
		{"exactly-at-max", "abcdef", 6, "abcdef"},
		{"one-over-max", "abcdefg", 6, "abcde…"},
		{
			"multiline-collapsed-then-untruncated",
			"line1\nline2",
			60,
			"line1 line2",
		},
		{
			"multiline-collapsed-then-truncated",
			"aaaaaaaaaa\nbbbbbbbbbb",
			10,
			"aaaaaaaaa…",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := truncateLine(tc.in, tc.max)
			if got != tc.want {
				t.Errorf("truncateLine(%q, %d) = %q; want %q", tc.in, tc.max, got, tc.want)
			}
			if strings.ContainsAny(got, "\n\r") {
				t.Errorf("truncateLine leaked newline/cr: %q", got)
			}
		})
	}
}

func TestSessionsCmd_FlagDefaults(t *testing.T) {
	cmd := newSessionsCmd()
	flags := cmd.Flags()

	jsonFlag := flags.Lookup("json")
	if jsonFlag == nil {
		t.Fatal("--json flag missing")
	}
	if jsonFlag.DefValue != "false" {
		t.Errorf("--json default = %q; want false", jsonFlag.DefValue)
	}

	statusFlag := flags.Lookup("status")
	if statusFlag == nil {
		t.Fatal("--status flag missing")
	}
	if statusFlag.DefValue != "" {
		t.Errorf("--status default = %q; want empty", statusFlag.DefValue)
	}

	limitFlag := flags.Lookup("limit")
	if limitFlag == nil {
		t.Fatal("--limit flag missing")
	}
	if limitFlag.DefValue != "50" {
		t.Errorf("--limit default = %q; want 50", limitFlag.DefValue)
	}

	offsetFlag := flags.Lookup("offset")
	if offsetFlag == nil {
		t.Fatal("--offset flag missing")
	}
	if offsetFlag.DefValue != "0" {
		t.Errorf("--offset default = %q; want 0", offsetFlag.DefValue)
	}
}

func TestSessionsCmd_ParsesFlags(t *testing.T) {
	cmd := newSessionsCmd()
	if err := cmd.ParseFlags([]string{"--limit", "5", "--offset", "10", "--status", "completed", "--json"}); err != nil {
		t.Fatalf("ParseFlags: %v", err)
	}
	want := map[string]string{
		"limit":  "5",
		"offset": "10",
		"status": "completed",
		"json":   "true",
	}
	for name, expect := range want {
		got := cmd.Flags().Lookup(name).Value.String()
		if got != expect {
			t.Errorf("flag %s = %q; want %q", name, got, expect)
		}
	}
}

// setupSessionsHome isolates HOME and UTA_HOME for newApp() so the sessions
// command operates against an empty global store under our control. Returns
// the state directory the command will read from.
func setupSessionsHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	utaHome := filepath.Join(home, ".uta")
	if err := os.MkdirAll(utaHome, 0o755); err != nil {
		t.Fatalf("mkdir uta home: %v", err)
	}
	t.Setenv("UTA_HOME", utaHome)

	// Chdir to a fresh non-project directory so newApp() can't auto-detect
	// some unrelated parent project.
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
	return utaHome
}

// runSessions builds a fresh sessions command and executes it with args.
func runSessions(t *testing.T, args ...string) (string, string, error) {
	t.Helper()
	cmd := newSessionsCmd()
	var out, errBuf bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errBuf)
	cmd.SetArgs(args)
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	err := cmd.ExecuteContext(context.Background())
	return out.String(), errBuf.String(), err
}

// seedSession inserts a session row directly via the store so the test does
// not depend on a full engine run.
func seedSession(t *testing.T, stateDir string, s store.Session) {
	t.Helper()
	st, err := store.Open(paths.DB(stateDir))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer st.Close()
	if err := st.CreateSession(s); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
}

// TestSessionsCmd_LastFlag verifies --last prints exactly one full session
// id (pipeable) and honors --status.
func TestSessionsCmd_LastFlag(t *testing.T) {
	stateDir := setupSessionsHome(t)

	base := time.Unix(1_700_000_000, 0).UTC()
	seedSession(t, stateDir, store.Session{
		ID: "sess-older-completed", Goal: "a", Worker: "w", Status: "completed", CreatedAt: base,
	})
	seedSession(t, stateDir, store.Session{
		ID: "sess-newer-failed", Goal: "b", Worker: "w", Status: "failed", CreatedAt: base.Add(time.Minute),
	})

	out, _, err := runSessions(t, "--last")
	if err != nil {
		t.Fatalf("sessions --last: %v", err)
	}
	if strings.TrimSpace(out) != "sess-newer-failed" {
		t.Errorf("--last = %q, want newest full id", strings.TrimSpace(out))
	}

	out, _, err = runSessions(t, "--last", "--status", "completed")
	if err != nil {
		t.Fatalf("sessions --last --status: %v", err)
	}
	if strings.TrimSpace(out) != "sess-older-completed" {
		t.Errorf("--last --status=completed = %q, want completed session", strings.TrimSpace(out))
	}
}

// TestSessionsCmd_LastFlagEmptyStore pins the error path: --last on a fresh
// store fails with a clear message instead of printing nothing.
func TestSessionsCmd_LastFlagEmptyStore(t *testing.T) {
	setupSessionsHome(t)
	_, _, err := runSessions(t, "--last")
	if err == nil || !strings.Contains(err.Error(), "no sessions") {
		t.Fatalf("want no-sessions error, got %v", err)
	}
}

// TestSessionsCmd_SinceFilter verifies --since drops sessions older than the
// window while keeping recent ones, and composes with --limit.
func TestSessionsCmd_SinceFilter(t *testing.T) {
	stateDir := setupSessionsHome(t)

	now := time.Now().UTC()
	seedSession(t, stateDir, store.Session{
		ID: "sess-ancient-run00", Goal: "old", Worker: "w", Status: "completed",
		CreatedAt: now.Add(-48 * time.Hour),
	})
	seedSession(t, stateDir, store.Session{
		ID: "sess-recent-run000", Goal: "new", Worker: "w", Status: "completed",
		CreatedAt: now.Add(-time.Hour),
	})

	out, _, err := runSessions(t, "--since", "24h")
	if err != nil {
		t.Fatalf("sessions --since: %v", err)
	}
	if strings.Contains(out, "sess-anc") {
		t.Errorf("--since 24h should drop the 48h-old session:\n%s", out)
	}
	if !strings.Contains(out, "sess-rec") {
		t.Errorf("--since 24h should keep the 1h-old session:\n%s", out)
	}

	// Bad duration surfaces a parse error.
	if _, _, err := runSessions(t, "--since", "nonsense"); err == nil {
		t.Error("want parse error for --since nonsense")
	}
}

// TestSessionsCmd_DurationColumn verifies the DURATION column renders the
// completed-created delta and "-" for unfinished sessions.
func TestSessionsCmd_DurationColumn(t *testing.T) {
	stateDir := setupSessionsHome(t)

	base := time.Unix(1_700_000_000, 0).UTC()
	seedSession(t, stateDir, store.Session{
		ID: "sess-finished-0001", Goal: "g", Worker: "w", Status: "completed",
		CreatedAt: base,
	})
	// CreateSession doesn't persist CompletedAt (that's MarkSession's job,
	// which stamps "now"); set a deterministic completion directly.
	{
		st, err := store.Open(paths.DB(stateDir))
		if err != nil {
			t.Fatalf("store.Open: %v", err)
		}
		_, err = st.DB.Exec(`UPDATE sessions SET completed_at = ? WHERE id = ?`,
			base.Add(3*time.Minute).UnixNano(), "sess-finished-0001")
		st.Close()
		if err != nil {
			t.Fatalf("set completed_at: %v", err)
		}
	}
	seedSession(t, stateDir, store.Session{
		ID: "sess-running-00001", Goal: "g", Worker: "w", Status: "running",
		CreatedAt: base.Add(time.Second),
	})

	out, _, err := runSessions(t)
	if err != nil {
		t.Fatalf("sessions: %v", err)
	}
	if !strings.Contains(out, "DURATION") {
		t.Errorf("missing DURATION header:\n%s", out)
	}
	if !strings.Contains(out, "3m0s") {
		t.Errorf("expected 3m0s duration for finished session:\n%s", out)
	}
	// The running session's duration cell renders as "-".
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "sess-run") && !strings.Contains(line, "-") {
			t.Errorf("running session should render '-' duration: %q", line)
		}
	}
}

func TestSessionsCmd_JSONOutput(t *testing.T) {
	stateDir := setupSessionsHome(t)

	base := time.Unix(1_700_000_000, 0).UTC()
	seedSession(t, stateDir, store.Session{
		ID:        "sess-0001-abcdefgh",
		Goal:      "first goal\nwith newline",
		Worker:    "claude",
		Status:    "completed",
		CreatedAt: base,
	})
	seedSession(t, stateDir, store.Session{
		ID:        "sess-0002-zzzzzzzz",
		Goal:      "second goal",
		Worker:    "gemini",
		Status:    "running",
		CreatedAt: base.Add(time.Second),
	})

	out, _, err := runSessions(t, "--json")
	if err != nil {
		t.Fatalf("sessions --json: %v", err)
	}

	var rows []struct {
		ID        string `json:"ID"`
		Goal      string `json:"Goal"`
		Worker    string `json:"Worker"`
		Status    string `json:"Status"`
		CreatedAt string `json:"CreatedAt"`
	}
	if err := json.Unmarshal([]byte(out), &rows); err != nil {
		t.Fatalf("decode JSON: %v\nraw=%s", err, out)
	}
	if len(rows) != 2 {
		t.Fatalf("rows: got %d want 2; raw=%s", len(rows), out)
	}
	// ListSessions returns newest-first.
	if rows[0].ID != "sess-0002-zzzzzzzz" || rows[1].ID != "sess-0001-abcdefgh" {
		t.Errorf("order wrong: got [%s, %s]", rows[0].ID, rows[1].ID)
	}
	if rows[0].Status != "running" || rows[1].Status != "completed" {
		t.Errorf("status round-trip wrong: %+v", rows)
	}
	// JSON preserves newlines in goal — only the table renderer collapses them.
	if !strings.Contains(rows[1].Goal, "\n") {
		t.Errorf("JSON output should preserve newlines in goal, got %q", rows[1].Goal)
	}
}

func TestSessionsCmd_StatusFilter(t *testing.T) {
	stateDir := setupSessionsHome(t)

	base := time.Unix(1_700_000_000, 0).UTC()
	seedSession(t, stateDir, store.Session{
		ID:        "sess-completed-id",
		Goal:      "done",
		Worker:    "claude",
		Status:    "completed",
		CreatedAt: base,
	})
	seedSession(t, stateDir, store.Session{
		ID:        "sess-failed-idxx",
		Goal:      "broke",
		Worker:    "claude",
		Status:    "failed",
		CreatedAt: base.Add(time.Second),
	})

	out, _, err := runSessions(t, "--json", "--status", "completed")
	if err != nil {
		t.Fatalf("sessions --status completed: %v", err)
	}
	var rows []store.Session
	if err := json.Unmarshal([]byte(out), &rows); err != nil {
		t.Fatalf("decode: %v\nraw=%s", err, out)
	}
	if len(rows) != 1 {
		t.Fatalf("filtered rows: got %d want 1; raw=%s", len(rows), out)
	}
	if rows[0].Status != "completed" || rows[0].ID != "sess-completed-id" {
		t.Errorf("filter returned wrong row: %+v", rows[0])
	}
}

func TestSessionsCmd_LimitAndOffset(t *testing.T) {
	stateDir := setupSessionsHome(t)

	base := time.Unix(1_700_000_000, 0).UTC()
	// Seed 3 rows with strictly increasing CreatedAt so order is deterministic.
	for i, id := range []string{"sess-aaa-id", "sess-bbb-id", "sess-ccc-id"} {
		seedSession(t, stateDir, store.Session{
			ID:        id,
			Goal:      id,
			Worker:    "claude",
			Status:    "completed",
			CreatedAt: base.Add(time.Duration(i) * time.Second),
		})
	}

	// limit=1 should give us the newest row only.
	out, _, err := runSessions(t, "--json", "--limit", "1")
	if err != nil {
		t.Fatalf("limit=1: %v", err)
	}
	var rows []store.Session
	if err := json.Unmarshal([]byte(out), &rows); err != nil {
		t.Fatalf("decode limit=1: %v\nraw=%s", err, out)
	}
	if len(rows) != 1 || rows[0].ID != "sess-ccc-id" {
		t.Errorf("limit=1: got %+v; want newest only", rows)
	}

	// offset=1 with limit=1 should give us the second-newest row.
	out, _, err = runSessions(t, "--json", "--limit", "1", "--offset", "1")
	if err != nil {
		t.Fatalf("offset=1: %v", err)
	}
	rows = nil
	if err := json.Unmarshal([]byte(out), &rows); err != nil {
		t.Fatalf("decode offset=1: %v\nraw=%s", err, out)
	}
	if len(rows) != 1 || rows[0].ID != "sess-bbb-id" {
		t.Errorf("offset=1 limit=1: got %+v; want sess-bbb-id", rows)
	}
}

func TestSessionsCmd_TableOutputCollapsesMultilineGoal(t *testing.T) {
	stateDir := setupSessionsHome(t)

	seedSession(t, stateDir, store.Session{
		ID:        "sess-multilineee",
		Goal:      "row-one\nrow-two\nrow-three",
		Worker:    "claude",
		Status:    "completed",
		CreatedAt: time.Unix(1_700_000_000, 0).UTC(),
	})

	out, _, err := runSessions(t)
	if err != nil {
		t.Fatalf("sessions: %v", err)
	}
	// Header should be present.
	for _, want := range []string{"ID", "CREATED", "WORKER", "STATUS", "GOAL"} {
		if !strings.Contains(out, want) {
			t.Errorf("header missing %q:\n%s", want, out)
		}
	}
	// Goal contained embedded newlines; the table cell must keep the row on
	// one line (i.e. truncateLine ran).
	dataLines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(dataLines) != 2 {
		t.Errorf("expected 2 output lines (header + 1 row), got %d:\n%s", len(dataLines), out)
	}
	if !strings.Contains(dataLines[1], "row-one row-two row-three") {
		t.Errorf("goal newlines not flattened in table row: %q", dataLines[1])
	}
	if !strings.Contains(dataLines[1], shortID("sess-multilineee")) {
		t.Errorf("shortID not used in table row: %q", dataLines[1])
	}
}
