package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/unleashtheagents/uta/internal/paths"
	"github.com/unleashtheagents/uta/internal/store"
)

func TestIfEmpty(t *testing.T) {
	cases := []struct {
		name     string
		in       string
		fallback string
		want     string
	}{
		{"empty-falls-back", "", "-", "-"},
		{"non-empty-passes-through", "abc", "-", "abc"},
		{"empty-empty-fallback", "", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ifEmpty(tc.in, tc.fallback); got != tc.want {
				t.Errorf("ifEmpty(%q, %q) = %q; want %q", tc.in, tc.fallback, got, tc.want)
			}
		})
	}
}

func TestSummarizePayload(t *testing.T) {
	cases := []struct {
		name    string
		payload string
		want    string
	}{
		{"empty-payload", "", ""},
		{
			"prefers-title-over-other-keys",
			`{"title":"plan complete","text":"ignored","status":"ok"}`,
			"title=plan complete",
		},
		{
			"falls-back-to-text-when-no-title-or-goal",
			`{"text":"hello world","reason":"ignored"}`,
			"text=hello world",
		},
		{
			"empty-string-title-skipped-uses-next-key",
			`{"title":"","goal":"do the thing"}`,
			"goal=do the thing",
		},
		{
			"numeric-chars-key",
			`{"chars":1234}`,
			"chars=1234",
		},
		{
			"max_parallel-numeric",
			`{"max_parallel":4}`,
			"max_parallel=4",
		},
		{
			"unknown-keys-falls-back-to-raw-json",
			`{"weird":"thing"}`,
			`{"weird":"thing"}`,
		},
		{
			"invalid-json-falls-back-to-raw-string",
			`not json at all`,
			"not json at all",
		},
		{
			"truncates-very-long-text",
			`{"text":"` + strings.Repeat("x", 200) + `"}`,
			"",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := summarizePayload(json.RawMessage(tc.payload))
			if tc.name == "truncates-very-long-text" {
				// truncateLine caps the rune count at max; the "…" sentinel
				// occupies the final rune slot.
				if runes := utf8.RuneCountInString(got); runes > 100 {
					t.Errorf("expected <=100 runes after truncation, got %d: %q", runes, got)
				}
				if !strings.HasPrefix(got, "text=") {
					t.Errorf("expected prefix text=, got %q", got)
				}
				if !strings.HasSuffix(got, "…") {
					t.Errorf("expected truncation marker at end, got %q", got)
				}
				return
			}
			if got != tc.want {
				t.Errorf("summarizePayload(%q) = %q; want %q", tc.payload, got, tc.want)
			}
		})
	}
}

func TestPrettyPrintTrajectory_HeaderAndRows(t *testing.T) {
	sess := store.Session{
		ID:     "sess-pretty-abcdefgh",
		Goal:   "investigate flake",
		Worker: "claude",
		Status: "completed",
	}
	ts := time.Date(2026, 5, 18, 14, 30, 45, 0, time.UTC)
	events := []store.EventRow{
		{
			Ts:        ts,
			Kind:      "session.start",
			SubtaskID: "",
			Payload:   json.RawMessage(`{"goal":"investigate flake"}`),
		},
		{
			Ts:        ts.Add(time.Second),
			Kind:      "subtask.completed",
			SubtaskID: "subtask-1234-xyzzy",
			Payload:   json.RawMessage(`{"status":"ok"}`),
		},
	}
	var buf bytes.Buffer
	if err := prettyPrintTrajectory(&buf, sess, events); err != nil {
		t.Fatalf("prettyPrintTrajectory: %v", err)
	}
	out := buf.String()

	// Header contains shortID, worker, status, goal.
	for _, want := range []string{
		"session " + shortID(sess.ID),
		"worker=claude",
		"status=completed",
		"goal=investigate flake",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("header missing %q:\n%s", want, out)
		}
	}

	// Each row should carry the timestamp, kind, and summarized payload.
	for _, want := range []string{
		ts.Format(time.TimeOnly),
		"session.start",
		"goal=investigate flake",
		ts.Add(time.Second).Format(time.TimeOnly),
		"subtask.completed",
		shortID("subtask-1234-xyzzy"),
		"status=ok",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("body missing %q:\n%s", want, out)
		}
	}

	// The event with an empty subtask id renders the "-" fallback from ifEmpty.
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	// header + blank + 2 event rows
	if len(lines) < 4 {
		t.Fatalf("expected >=4 lines (header, blank, two rows), got %d:\n%s", len(lines), out)
	}
	if !strings.Contains(lines[2], " -  ") && !strings.Contains(lines[2], " - ") {
		t.Errorf("first event row should show '-' for empty subtask id: %q", lines[2])
	}
}

func TestTrajectoryCmd_FlagDefaults(t *testing.T) {
	cmd := newTrajectoryCmd()
	flags := cmd.Flags()

	formatFlag := flags.Lookup("format")
	if formatFlag == nil {
		t.Fatal("--format flag missing")
	}
	if formatFlag.DefValue != "pretty" {
		t.Errorf("--format default = %q; want pretty", formatFlag.DefValue)
	}

	limitFlag := flags.Lookup("limit")
	if limitFlag == nil {
		t.Fatal("--limit flag missing")
	}
	if limitFlag.DefValue != "0" {
		t.Errorf("--limit default = %q; want 0", limitFlag.DefValue)
	}

	offsetFlag := flags.Lookup("offset")
	if offsetFlag == nil {
		t.Fatal("--offset flag missing")
	}
	if offsetFlag.DefValue != "0" {
		t.Errorf("--offset default = %q; want 0", offsetFlag.DefValue)
	}
}

func TestTrajectoryCmd_RequiresSessionArg(t *testing.T) {
	cmd := newTrajectoryCmd()
	var out, errBuf bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errBuf)
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	cmd.SetArgs(nil)
	if err := cmd.ExecuteContext(context.Background()); err == nil {
		t.Fatalf("expected error when no session id passed, got nil")
	}
}

// runTrajectory builds a fresh trajectory command and executes it with args.
func runTrajectory(t *testing.T, args ...string) (string, string, error) {
	t.Helper()
	cmd := newTrajectoryCmd()
	var out, errBuf bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errBuf)
	cmd.SetArgs(args)
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	err := cmd.ExecuteContext(context.Background())
	return out.String(), errBuf.String(), err
}

// seedEvents inserts a session plus a list of events directly into the store
// at stateDir. The events are inserted in slice order with monotonically
// increasing seq values starting at 1.
func seedEvents(t *testing.T, stateDir string, sess store.Session, events []store.EventRow) {
	t.Helper()
	st, err := store.Open(paths.DB(stateDir))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer st.Close()
	if err := st.CreateSession(sess); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	for i, ev := range events {
		if err := st.InsertEvent(sess.ID, ev.SubtaskID, int64(i+1), ev.Ts, ev.Kind, ev.Payload); err != nil {
			t.Fatalf("InsertEvent[%d]: %v", i, err)
		}
	}
}

func TestTrajectoryCmd_SessionNotFound(t *testing.T) {
	setupSessionsHome(t)
	_, _, err := runTrajectory(t, "no-such-session-id")
	if err == nil {
		t.Fatalf("expected error for missing session, got nil")
	}
	if !strings.Contains(err.Error(), "session not found") {
		t.Errorf("error should say 'session not found', got: %v", err)
	}
}

func TestTrajectoryCmd_UnknownFormat(t *testing.T) {
	stateDir := setupSessionsHome(t)
	seedEvents(t, stateDir, store.Session{
		ID:        "sess-fmt-aaaaaaaa",
		Goal:      "g",
		Worker:    "claude",
		Status:    "completed",
		CreatedAt: time.Unix(1_700_000_000, 0).UTC(),
	}, nil)
	_, _, err := runTrajectory(t, "sess-fmt-aaaaaaaa", "--format", "yaml")
	if err == nil {
		t.Fatalf("expected error for unknown format, got nil")
	}
	if !strings.Contains(err.Error(), "unknown format") {
		t.Errorf("error should mention 'unknown format': %v", err)
	}
}

func TestTrajectoryCmd_JSONOutput(t *testing.T) {
	stateDir := setupSessionsHome(t)
	sessID := "sess-json-trajec"
	ts := time.Unix(1_700_000_000, 0).UTC()
	seedEvents(t, stateDir, store.Session{
		ID:        sessID,
		Goal:      "json goal",
		Worker:    "claude",
		Status:    "completed",
		CreatedAt: ts,
	}, []store.EventRow{
		{Ts: ts, Kind: "session.start", Payload: json.RawMessage(`{"goal":"json goal"}`)},
		{Ts: ts.Add(time.Second), Kind: "subtask.completed", SubtaskID: "st-1", Payload: json.RawMessage(`{"status":"ok"}`)},
	})

	out, _, err := runTrajectory(t, sessID, "--format", "json")
	if err != nil {
		t.Fatalf("trajectory --format json: %v", err)
	}
	var rows []store.EventRow
	if err := json.Unmarshal([]byte(out), &rows); err != nil {
		t.Fatalf("decode JSON: %v\nraw=%s", err, out)
	}
	if len(rows) != 2 {
		t.Fatalf("rows: got %d want 2; raw=%s", len(rows), out)
	}
	// ListEvents orders by seq ASC, so first inserted = first returned.
	if rows[0].Kind != "session.start" || rows[1].Kind != "subtask.completed" {
		t.Errorf("event order wrong: %+v", rows)
	}
	if rows[1].SubtaskID != "st-1" {
		t.Errorf("subtask id not round-tripped: %+v", rows[1])
	}
}

func TestTrajectoryCmd_JSONLOutput(t *testing.T) {
	stateDir := setupSessionsHome(t)
	sessID := "sess-jsonl-trajc"
	ts := time.Unix(1_700_000_000, 0).UTC()
	seedEvents(t, stateDir, store.Session{
		ID:        sessID,
		Goal:      "jsonl goal",
		Worker:    "claude",
		Status:    "completed",
		CreatedAt: ts,
	}, []store.EventRow{
		{Ts: ts, Kind: "session.start", Payload: json.RawMessage(`{"goal":"jsonl goal"}`)},
		{Ts: ts.Add(time.Second), Kind: "subtask.completed", Payload: json.RawMessage(`{"status":"ok"}`)},
	})

	out, _, err := runTrajectory(t, sessID, "--format", "jsonl")
	if err != nil {
		t.Fatalf("trajectory --format jsonl: %v", err)
	}
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("expected 2 jsonl rows, got %d:\n%s", len(lines), out)
	}
	for i, line := range lines {
		var ev store.EventRow
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			t.Fatalf("line %d not valid JSON: %v\nline=%s", i, err, line)
		}
	}
}

func TestTrajectoryCmd_PrettyOutput(t *testing.T) {
	stateDir := setupSessionsHome(t)
	sessID := "sess-pretty-traje"
	ts := time.Unix(1_700_000_000, 0).UTC()
	seedEvents(t, stateDir, store.Session{
		ID:        sessID,
		Goal:      "pretty goal",
		Worker:    "claude",
		Status:    "completed",
		CreatedAt: ts,
	}, []store.EventRow{
		{Ts: ts, Kind: "session.start", Payload: json.RawMessage(`{"goal":"pretty goal"}`)},
	})

	// Default format is pretty.
	out, _, err := runTrajectory(t, sessID)
	if err != nil {
		t.Fatalf("trajectory (default pretty): %v", err)
	}
	for _, want := range []string{
		"session " + shortID(sessID),
		"worker=claude",
		"status=completed",
		"goal=pretty goal",
		"session.start",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("pretty output missing %q:\n%s", want, out)
		}
	}
}

func TestTrajectoryCmd_LimitAndOffset(t *testing.T) {
	stateDir := setupSessionsHome(t)
	sessID := "sess-page-trajec"
	ts := time.Unix(1_700_000_000, 0).UTC()
	events := []store.EventRow{
		{Ts: ts, Kind: "k1", Payload: json.RawMessage(`{"text":"first"}`)},
		{Ts: ts.Add(time.Second), Kind: "k2", Payload: json.RawMessage(`{"text":"second"}`)},
		{Ts: ts.Add(2 * time.Second), Kind: "k3", Payload: json.RawMessage(`{"text":"third"}`)},
	}
	seedEvents(t, stateDir, store.Session{
		ID:        sessID,
		Goal:      "g",
		Worker:    "claude",
		Status:    "completed",
		CreatedAt: ts,
	}, events)

	// limit=1 should yield the first event only.
	out, _, err := runTrajectory(t, sessID, "--format", "json", "--limit", "1")
	if err != nil {
		t.Fatalf("limit=1: %v", err)
	}
	var rows []store.EventRow
	if err := json.Unmarshal([]byte(out), &rows); err != nil {
		t.Fatalf("decode: %v\nraw=%s", err, out)
	}
	if len(rows) != 1 || rows[0].Kind != "k1" {
		t.Errorf("limit=1: expected [k1], got %+v", rows)
	}

	// offset=1 with limit=1 should yield the second event.
	out, _, err = runTrajectory(t, sessID, "--format", "json", "--limit", "1", "--offset", "1")
	if err != nil {
		t.Fatalf("offset=1: %v", err)
	}
	rows = nil
	if err := json.Unmarshal([]byte(out), &rows); err != nil {
		t.Fatalf("decode: %v\nraw=%s", err, out)
	}
	if len(rows) != 1 || rows[0].Kind != "k2" {
		t.Errorf("offset=1 limit=1: expected [k2], got %+v", rows)
	}
}
