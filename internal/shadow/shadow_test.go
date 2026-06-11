package shadow

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/unleashtheagents/uta/internal/profile"
	"github.com/unleashtheagents/uta/internal/store"
)

func newTestStore(t *testing.T) (*store.Store, *store.Blobs) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "shadow-test.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st, store.NewBlobs(dir)
}

func seedSession(t *testing.T, st *store.Store, blobs *store.Blobs, id, goal, mode, body, status string, createdAt time.Time) {
	t.Helper()
	ref, err := blobs.Put([]byte(body), "txt")
	if err != nil {
		t.Fatalf("blobs.Put: %v", err)
	}
	sess := store.Session{
		ID:        id,
		Goal:      goal,
		Worker:    "claude",
		Status:    "running",
		CreatedAt: createdAt,
		ModeName:  mode,
	}
	if err := st.CreateSession(sess); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	// MarkSession overrides completed_at to time.Now(), which is fine for
	// these tests — we only assert on created_at filtering.
	if err := st.MarkSession(id, status, ref); err != nil {
		t.Fatalf("MarkSession: %v", err)
	}
}

func TestCountTokens(t *testing.T) {
	cases := []struct {
		in   string
		want int
	}{
		{"", 0},
		{"hello", 1},
		{"hello world", 2},
		{"  many   spaces  here  ", 3},
		{"line one\nline two\n", 4},
		{"\t\n  ", 0},
	}
	for _, tc := range cases {
		if got := countTokens(tc.in); got != tc.want {
			t.Errorf("countTokens(%q) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

func TestUnifiedDiff(t *testing.T) {
	if d := unifiedDiff("same\nlines\n", "same\nlines\n"); d != "" {
		t.Errorf("unchanged diff should be empty, got %q", d)
	}
	d := unifiedDiff("a\nb\nc\n", "a\nB\nc\n")
	if !strings.Contains(d, "- b") || !strings.Contains(d, "+ B") {
		t.Errorf("expected +/- lines, got:\n%s", d)
	}
}

func TestShadowRun_ProducesDelta(t *testing.T) {
	st, blobs := newTestStore(t)
	now := time.Now()
	seedSession(t, st, blobs, "sess-1", "draft the README",
		"audit", "the original final answer with five words", "completed", now.Add(-time.Hour))

	deps := Deps{
		Store: st,
		Blobs: blobs,
		Replay: func(ctx context.Context, in ReplayInput) (ReplayOutput, error) {
			return ReplayOutput{
				FinalText: "the original final answer with five words and some extra words appended for drift testing purposes here",
				Status:    "completed",
				Duration:  50 * time.Millisecond,
			}, nil
		},
	}
	after := &profile.MissionProfile{Name: "audit"}
	sh := Shadow{
		SourceSessionID: "sess-1",
		ProfileAfter:    after,
		DriftTokens:     5,
	}
	report, err := sh.Run(context.Background(), deps)
	if err != nil {
		t.Fatalf("Shadow.Run: %v", err)
	}
	if len(report.Sessions) != 1 {
		t.Fatalf("want 1 session in report, got %d", len(report.Sessions))
	}
	d := report.Sessions[0]
	if d.OriginalTokens != 7 {
		t.Errorf("original tokens = %d, want 7", d.OriginalTokens)
	}
	if d.NewTokens <= d.OriginalTokens {
		t.Errorf("new tokens (%d) should exceed original (%d)", d.NewTokens, d.OriginalTokens)
	}
	if !d.Drifted {
		t.Errorf("expected drift flag, got delta=%d threshold=%d", d.TokenDelta, sh.DriftTokens)
	}
	if d.Diff == "" {
		t.Errorf("expected non-empty diff")
	}
	if report.DriftedCount() != 1 {
		t.Errorf("drifted count = %d, want 1", report.DriftedCount())
	}
}

func TestShadowRun_StatusChangeMarksDrift(t *testing.T) {
	st, blobs := newTestStore(t)
	seedSession(t, st, blobs, "sess-2", "fix bug",
		"audit", "identical answer", "completed", time.Now().Add(-time.Hour))

	deps := Deps{
		Store: st,
		Blobs: blobs,
		Replay: func(ctx context.Context, in ReplayInput) (ReplayOutput, error) {
			return ReplayOutput{
				FinalText: "identical answer",
				Status:    "failed",
			}, nil
		},
	}
	sh := Shadow{
		SourceSessionID: "sess-2",
		ProfileAfter:    &profile.MissionProfile{Name: "audit"},
	}
	report, err := sh.Run(context.Background(), deps)
	if err != nil {
		t.Fatalf("Shadow.Run: %v", err)
	}
	d := report.Sessions[0]
	if !d.Drifted {
		t.Errorf("status change completed→failed should flag drift")
	}
	if d.TokenDelta != 0 {
		t.Errorf("token delta = %d, want 0", d.TokenDelta)
	}
}

func TestRunMode_FiltersBySince(t *testing.T) {
	st, blobs := newTestStore(t)
	now := time.Now()
	seedSession(t, st, blobs, "recent", "g1", "audit", "recent body", "completed", now.Add(-2*time.Hour))
	seedSession(t, st, blobs, "old", "g2", "audit", "old body", "completed", now.Add(-48*time.Hour))
	seedSession(t, st, blobs, "off-mode", "g3", "dev", "off body", "completed", now.Add(-1*time.Hour))

	called := map[string]bool{}
	deps := Deps{
		Store: st,
		Blobs: blobs,
		Replay: func(ctx context.Context, in ReplayInput) (ReplayOutput, error) {
			called[in.Goal] = true
			return ReplayOutput{FinalText: "new body", Status: "completed"}, nil
		},
	}
	after := &profile.MissionProfile{Name: "audit"}
	report, err := RunMode(context.Background(), deps, nil, after, now.Add(-3*time.Hour), Options{MaxSessions: 10})
	if err != nil {
		t.Fatalf("RunMode: %v", err)
	}
	if len(report.Sessions) != 1 {
		t.Fatalf("want 1 session, got %d", len(report.Sessions))
	}
	if report.Sessions[0].SessionID != "recent" {
		t.Errorf("unexpected session in report: %+v", report.Sessions[0])
	}
	if called["g2"] || called["g3"] {
		t.Errorf("replay called for filtered-out sessions: %v", called)
	}
}

func TestRunMode_HonorsContextCancel(t *testing.T) {
	st, blobs := newTestStore(t)
	now := time.Now()
	seedSession(t, st, blobs, "a", "ga", "audit", "body a", "completed", now.Add(-2*time.Hour))
	seedSession(t, st, blobs, "b", "gb", "audit", "body b", "completed", now.Add(-1*time.Hour))

	ctx, cancel := context.WithCancel(context.Background())
	count := 0
	deps := Deps{
		Store: st,
		Blobs: blobs,
		Replay: func(ctx context.Context, in ReplayInput) (ReplayOutput, error) {
			count++
			cancel()
			return ReplayOutput{FinalText: "", Status: "completed"}, nil
		},
	}
	report, err := RunMode(ctx, deps, nil, &profile.MissionProfile{Name: "audit"}, time.Time{}, Options{MaxSessions: 10})
	if err != nil {
		t.Fatalf("RunMode: %v", err)
	}
	if count != 1 {
		t.Errorf("want exactly 1 replay before cancel, got %d", count)
	}
	if len(report.Sessions) != 1 {
		t.Errorf("want 1 session in report, got %d", len(report.Sessions))
	}
}

func TestReport_Markdown_RendersHeader(t *testing.T) {
	r := &Report{
		GeneratedAt:    time.Date(2026, 5, 18, 12, 0, 0, 0, time.UTC),
		ProfileAfter:   "audit",
		ProfileBefore:  "audit",
		DriftThreshold: 50,
		Sessions: []SessionDelta{{
			SessionID:      "abcdef1234",
			Goal:           "demo",
			OriginalStatus: "completed",
			NewStatus:      "completed",
			OriginalTokens: 10,
			NewTokens:      12,
			TokenDelta:     2,
			Drifted:        false,
			Diff:           "  same\n",
			ReplayDuration: 100 * time.Millisecond,
		}},
	}
	md := r.Markdown()
	if !strings.Contains(md, "Shadow report") {
		t.Errorf("missing header: %s", md)
	}
	if !strings.Contains(md, "abcdef12") {
		t.Errorf("missing short id: %s", md)
	}
	if !strings.Contains(md, "Drift threshold: 50") {
		t.Errorf("missing drift threshold line: %s", md)
	}
}

func TestShadow_Run_MissingSession(t *testing.T) {
	st, blobs := newTestStore(t)
	deps := Deps{Store: st, Blobs: blobs, Replay: func(ctx context.Context, in ReplayInput) (ReplayOutput, error) {
		return ReplayOutput{}, nil
	}}
	sh := Shadow{
		SourceSessionID: "nope",
		ProfileAfter:    &profile.MissionProfile{Name: "audit"},
	}
	if _, err := sh.Run(context.Background(), deps); err == nil {
		t.Errorf("expected error for missing session")
	}
}

func TestEffectiveDriftThreshold(t *testing.T) {
	cases := []struct {
		name     string
		override int
		profile  *profile.MissionProfile
		want     int
	}{
		{"all-defaults", 0, nil, DefaultDriftTokens},
		{"profile-set", 0, &profile.MissionProfile{Name: "p", ShadowDriftTokens: 12}, 12},
		{"override-wins-over-profile", 7, &profile.MissionProfile{Name: "p", ShadowDriftTokens: 12}, 7},
		{"override-wins-over-default", 7, nil, 7},
		{"zero-profile-falls-through", 0, &profile.MissionProfile{Name: "p"}, DefaultDriftTokens},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := effectiveDriftThreshold(tc.override, tc.profile); got != tc.want {
				t.Errorf("got %d, want %d", got, tc.want)
			}
		})
	}
}

func TestShadowRun_HonorsProfileDriftTokens(t *testing.T) {
	st, blobs := newTestStore(t)
	seedSession(t, st, blobs, "sess-p", "g", "audit",
		"one two three four five six seven", "completed", time.Now().Add(-time.Hour))

	deps := Deps{
		Store: st, Blobs: blobs,
		Replay: func(ctx context.Context, in ReplayInput) (ReplayOutput, error) {
			// Adds exactly 3 tokens of drift.
			return ReplayOutput{FinalText: "one two three four five six seven plus ten more", Status: "completed"}, nil
		},
	}
	after := &profile.MissionProfile{Name: "audit", ShadowDriftTokens: 2}
	sh := Shadow{SourceSessionID: "sess-p", ProfileAfter: after}
	report, err := sh.Run(context.Background(), deps)
	if err != nil {
		t.Fatalf("Shadow.Run: %v", err)
	}
	if report.DriftThreshold != 2 {
		t.Errorf("report drift threshold = %d, want 2 (from profile)", report.DriftThreshold)
	}
	if !report.Sessions[0].Drifted {
		t.Errorf("expected drift flag when delta>=profile threshold")
	}
}

func TestShadowRun_OverrideBeatsProfileDriftTokens(t *testing.T) {
	st, blobs := newTestStore(t)
	seedSession(t, st, blobs, "sess-o", "g", "audit",
		"one two three four five", "completed", time.Now().Add(-time.Hour))

	deps := Deps{
		Store: st, Blobs: blobs,
		Replay: func(ctx context.Context, in ReplayInput) (ReplayOutput, error) {
			// Drift of exactly 3 tokens.
			return ReplayOutput{FinalText: "one two three four five six seven eight", Status: "completed"}, nil
		},
	}
	after := &profile.MissionProfile{Name: "audit", ShadowDriftTokens: 1}
	sh := Shadow{
		SourceSessionID: "sess-o",
		ProfileAfter:    after,
		DriftTokens:     100, // big enough to mask the 3-token delta
	}
	report, err := sh.Run(context.Background(), deps)
	if err != nil {
		t.Fatalf("Shadow.Run: %v", err)
	}
	if report.DriftThreshold != 100 {
		t.Errorf("report drift threshold = %d, want 100 (from override)", report.DriftThreshold)
	}
	if report.Sessions[0].Drifted {
		t.Errorf("did not expect drift: explicit override should win over profile")
	}
}

func TestSandboxWorkdir_ParallelRunsDoNotCollide(t *testing.T) {
	a, err := SandboxWorkdir()
	if err != nil {
		t.Fatalf("SandboxWorkdir #1: %v", err)
	}
	defer func() { _ = os.RemoveAll(a) }()
	b, err := SandboxWorkdir()
	if err != nil {
		t.Fatalf("SandboxWorkdir #2: %v", err)
	}
	defer func() { _ = os.RemoveAll(b) }()
	if a == b {
		t.Fatalf("two SandboxWorkdir() calls returned the same path: %q", a)
	}
}

// TestSandboxWorkdir_EmptyWhenNoProjectRoot asserts the back-compat
// path: SandboxWorkdir with no args (or empty string) returns an empty
// tempdir, matching the v1 behavior tests depend on.
func TestSandboxWorkdir_EmptyWhenNoProjectRoot(t *testing.T) {
	dir, err := SandboxWorkdir()
	if err != nil {
		t.Fatalf("SandboxWorkdir(): %v", err)
	}
	defer os.RemoveAll(dir)
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("expected empty sandbox, got %d entries", len(entries))
	}
}

// TestSandboxWorkdir_CopiesProject covers the new staging behavior: when
// projectRoot is supplied, regular files are reproduced in the sandbox
// with their relative paths preserved, and the skip-list (.git, .uta,
// node_modules) is honored. Closes the audit gap that flagged
// SandboxWorkdir() as returning an empty dir when the spec called for a
// "sandboxed copy".
func TestSandboxWorkdir_CopiesProject(t *testing.T) {
	src := t.TempDir()
	// Files under various paths — a top-level file, a nested file, and a
	// file inside each skip target.
	mustWrite := func(rel, content string) {
		t.Helper()
		full := filepath.Join(src, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("MkdirAll: %v", err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
	}
	mustWrite("top.txt", "hello")
	mustWrite("nested/dir/inner.md", "deep")
	mustWrite(".git/config", "should-not-copy")
	mustWrite(".uta/project.yaml", "should-not-copy")
	mustWrite("node_modules/foo/index.js", "should-not-copy")

	dst, err := SandboxWorkdir(src)
	if err != nil {
		t.Fatalf("SandboxWorkdir(%q): %v", src, err)
	}
	defer os.RemoveAll(dst)

	mustExist := func(rel string) {
		t.Helper()
		if _, err := os.Stat(filepath.Join(dst, rel)); err != nil {
			t.Errorf("expected %s to exist in sandbox: %v", rel, err)
		}
	}
	mustNotExist := func(rel string) {
		t.Helper()
		if _, err := os.Stat(filepath.Join(dst, rel)); err == nil {
			t.Errorf("expected %s NOT to exist in sandbox", rel)
		}
	}
	mustExist("top.txt")
	mustExist("nested/dir/inner.md")
	mustNotExist(".git/config")
	mustNotExist(".uta/project.yaml")
	mustNotExist("node_modules/foo/index.js")

	// Content of copied files must match.
	body, err := os.ReadFile(filepath.Join(dst, "top.txt"))
	if err != nil {
		t.Fatalf("ReadFile top.txt: %v", err)
	}
	if string(body) != "hello" {
		t.Errorf("top.txt content = %q, want %q", body, "hello")
	}
}

func TestSelectSessions(t *testing.T) {
	st, blobs := newTestStore(t)
	now := time.Now()
	seedSession(t, st, blobs, "old", "g-old", "audit", "old", "completed", now.Add(-48*time.Hour))
	seedSession(t, st, blobs, "mid", "g-mid", "audit", "mid", "completed", now.Add(-12*time.Hour))
	seedSession(t, st, blobs, "new", "g-new", "audit", "new", "completed", now.Add(-1*time.Hour))
	seedSession(t, st, blobs, "off", "g-off", "dev", "off", "completed", now.Add(-2*time.Hour))

	got, err := SelectSessions(st, "audit", now.Add(-24*time.Hour), 10)
	if err != nil {
		t.Fatalf("SelectSessions: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 sessions inside the 24h window, got %d", len(got))
	}
	// Oldest-first ordering for chronological reports.
	if got[0].ID != "mid" || got[1].ID != "new" {
		t.Errorf("expected [mid, new], got [%s, %s]", got[0].ID, got[1].ID)
	}

	// No-store guard.
	if _, err := SelectSessions(nil, "audit", time.Time{}, 5); err == nil {
		t.Errorf("expected error for nil store")
	}

	// Zero since disables the time filter.
	all, err := SelectSessions(st, "audit", time.Time{}, 10)
	if err != nil {
		t.Fatalf("SelectSessions (no cutoff): %v", err)
	}
	if len(all) != 3 {
		t.Errorf("want 3 audit sessions when since is zero, got %d", len(all))
	}
}

func TestShadow_Run_ReplayError_FlagsDrift(t *testing.T) {
	st, blobs := newTestStore(t)
	seedSession(t, st, blobs, "sess-err", "g", "audit", "orig", "completed", time.Now().Add(-time.Hour))
	deps := Deps{
		Store: st, Blobs: blobs,
		Replay: func(ctx context.Context, in ReplayInput) (ReplayOutput, error) {
			return ReplayOutput{}, context.DeadlineExceeded
		},
	}
	sh := Shadow{SourceSessionID: "sess-err", ProfileAfter: &profile.MissionProfile{Name: "audit"}}
	report, err := sh.Run(context.Background(), deps)
	if err != nil {
		t.Fatalf("Shadow.Run returned err: %v", err)
	}
	d := report.Sessions[0]
	if d.ReplayErr == "" {
		t.Errorf("expected ReplayErr to be set")
	}
	if !d.Drifted {
		t.Errorf("expected replay error to mark session as drifted")
	}
}
