package store

import (
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestCreateAndGetSession(t *testing.T) {
	s := newTestStore(t)

	created := time.Unix(1_700_000_000, 0).UTC()
	in := Session{
		ID:           "sess-1",
		Goal:         "do the thing",
		Worker:       "claude",
		Planner:      "claude",
		Status:       "running",
		CreatedAt:    created,
		WorkflowPath: "/tmp/wf.yaml",
		MetaJSON:     `{"k":"v"}`,
	}
	if err := s.CreateSession(in); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	got, err := s.GetSession("sess-1")
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if got.ID != in.ID || got.Goal != in.Goal || got.Worker != in.Worker ||
		got.Planner != in.Planner || got.Status != in.Status ||
		got.WorkflowPath != in.WorkflowPath || got.MetaJSON != in.MetaJSON {
		t.Fatalf("round-trip mismatch:\n got=%+v\nwant=%+v", got, in)
	}
	if !got.CreatedAt.Equal(created) {
		t.Fatalf("CreatedAt: got %v want %v", got.CreatedAt, created)
	}
	if got.CompletedAt != nil {
		t.Fatalf("CompletedAt: expected nil, got %v", *got.CompletedAt)
	}
	if got.FinalAnswerRef != "" {
		t.Fatalf("FinalAnswerRef: expected empty, got %q", got.FinalAnswerRef)
	}
}

func TestCreateSession_NullableFieldsAndMetaDefault(t *testing.T) {
	s := newTestStore(t)

	created := time.Unix(1_700_000_001, 0).UTC()
	// Planner, WorkflowPath, MetaJSON intentionally empty.
	in := Session{
		ID:        "sess-2",
		Goal:      "bare",
		Worker:    "gemini",
		Status:    "running",
		CreatedAt: created,
	}
	if err := s.CreateSession(in); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	// Verify the underlying row stores NULLs for the optional fields and
	// substitutes the default JSON object for meta_json.
	var planner, workflowPath sql.NullString
	var metaJSON string
	err := s.DB.QueryRow(
		`SELECT planner, workflow_path, meta_json FROM sessions WHERE id = ?`, in.ID,
	).Scan(&planner, &workflowPath, &metaJSON)
	if err != nil {
		t.Fatalf("scan raw row: %v", err)
	}
	if planner.Valid {
		t.Fatalf("planner: expected NULL, got %q", planner.String)
	}
	if workflowPath.Valid {
		t.Fatalf("workflow_path: expected NULL, got %q", workflowPath.String)
	}
	if metaJSON != "{}" {
		t.Fatalf("meta_json: expected default %q, got %q", "{}", metaJSON)
	}

	// And the high-level getter should normalize NULLs back to empty strings.
	got, err := s.GetSession(in.ID)
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if got.Planner != "" || got.WorkflowPath != "" {
		t.Fatalf("expected empty optional strings, got planner=%q workflow=%q",
			got.Planner, got.WorkflowPath)
	}
	if got.MetaJSON != "{}" {
		t.Fatalf("MetaJSON default: got %q", got.MetaJSON)
	}
}

func TestGetSession_NotFound(t *testing.T) {
	s := newTestStore(t)
	_, err := s.GetSession("missing")
	if !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("expected sql.ErrNoRows, got %v", err)
	}
}

func TestCreateAndUpdateSubtask(t *testing.T) {
	s := newTestStore(t)

	// Subtasks require a parent session row (FK enforced).
	if err := s.CreateSession(Session{
		ID:        "sess-3",
		Goal:      "parent",
		Worker:    "claude",
		Status:    "running",
		CreatedAt: time.Unix(1_700_000_002, 0).UTC(),
	}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	pending := Subtask{
		ID:        "st-1",
		SessionID: "sess-3",
		Ord:       0,
		Title:     "first",
		PromptRef: "blob:abc",
		Worker:    "claude",
		Status:    "pending",
	}
	if err := s.CreateSubtask(pending); err != nil {
		t.Fatalf("CreateSubtask: %v", err)
	}

	// Read it back via SubtaskListBySession and assert pending-state fields
	// are zero-valued / nil as expected.
	subs, err := s.SubtaskListBySession("sess-3", 0, 0)
	if err != nil {
		t.Fatalf("SubtaskListBySession: %v", err)
	}
	if len(subs) != 1 {
		t.Fatalf("expected 1 subtask, got %d", len(subs))
	}
	if subs[0].StartedAt != nil || subs[0].CompletedAt != nil {
		t.Fatalf("expected nil timestamps on pending subtask, got %+v", subs[0])
	}
	if subs[0].ResultText != "" || subs[0].Error != "" || subs[0].ProviderSessionID != "" {
		t.Fatalf("expected empty optional fields on pending subtask, got %+v", subs[0])
	}

	// Now apply terminal-state update.
	started := time.Unix(1_700_000_010, 0).UTC()
	completed := time.Unix(1_700_000_020, 0).UTC()
	done := Subtask{
		ID:                "st-1",
		ProviderSessionID: "prov-xyz",
		Status:            "ok",
		StartedAt:         &started,
		CompletedAt:       &completed,
		ResultText:        "the result",
		RawOutputRef:      "blob:def",
	}
	if err := s.UpdateSubtask(done); err != nil {
		t.Fatalf("UpdateSubtask: %v", err)
	}

	subs, err = s.SubtaskListBySession("sess-3", 0, 0)
	if err != nil {
		t.Fatalf("SubtaskListBySession after update: %v", err)
	}
	if len(subs) != 1 {
		t.Fatalf("expected 1 subtask after update, got %d", len(subs))
	}
	g := subs[0]
	if g.Status != "ok" || g.ProviderSessionID != "prov-xyz" ||
		g.ResultText != "the result" || g.RawOutputRef != "blob:def" {
		t.Fatalf("update did not persist as expected: %+v", g)
	}
	if g.StartedAt == nil || !g.StartedAt.Equal(started) {
		t.Fatalf("StartedAt: got %v want %v", g.StartedAt, started)
	}
	if g.CompletedAt == nil || !g.CompletedAt.Equal(completed) {
		t.Fatalf("CompletedAt: got %v want %v", g.CompletedAt, completed)
	}
	// Error fields cleared on successful terminal update.
	if g.Error != "" || g.ErrorKind != "" {
		t.Fatalf("expected cleared error fields, got error=%q kind=%q", g.Error, g.ErrorKind)
	}
}

func TestUpdateSubtask_NullableTimestampsAndErrorFields(t *testing.T) {
	s := newTestStore(t)
	if err := s.CreateSession(Session{
		ID:        "sess-4",
		Goal:      "parent",
		Worker:    "claude",
		Status:    "running",
		CreatedAt: time.Unix(1_700_000_003, 0).UTC(),
	}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if err := s.CreateSubtask(Subtask{
		ID: "st-2", SessionID: "sess-4", Ord: 0,
		Title: "boom", PromptRef: "blob:x", Worker: "claude", Status: "pending",
	}); err != nil {
		t.Fatalf("CreateSubtask: %v", err)
	}

	// Failed terminal state: no timestamps, error fields populated.
	if err := s.UpdateSubtask(Subtask{
		ID:        "st-2",
		Status:    "error",
		Error:     "exit 1",
		ErrorKind: "provider",
	}); err != nil {
		t.Fatalf("UpdateSubtask: %v", err)
	}

	// Verify raw row stores NULLs for the unset timestamp fields.
	var startedAt, completedAt sql.NullInt64
	var providerSessionID, resultText, errStr, errKind sql.NullString
	err := s.DB.QueryRow(
		`SELECT started_at, completed_at, provider_session_id, result_text, error, error_kind
		   FROM subtasks WHERE id = ?`, "st-2",
	).Scan(&startedAt, &completedAt, &providerSessionID, &resultText, &errStr, &errKind)
	if err != nil {
		t.Fatalf("scan raw subtask: %v", err)
	}
	if startedAt.Valid || completedAt.Valid {
		t.Fatalf("expected NULL timestamps, got started=%v completed=%v", startedAt, completedAt)
	}
	if providerSessionID.Valid || resultText.Valid {
		t.Fatalf("expected NULL provider/result fields, got prov=%v res=%v", providerSessionID, resultText)
	}
	if !errStr.Valid || errStr.String != "exit 1" {
		t.Fatalf("error field: got %v want %q", errStr, "exit 1")
	}
	if !errKind.Valid || errKind.String != "provider" {
		t.Fatalf("error_kind: got %v want %q", errKind, "provider")
	}
}

// TestCostRollups verifies the per-(day, mode, provider) usage rollup
// the new `uta perf --cost` and `uta dash` cost pane both consume. The
// query lives in dao.go; this test pins its grouping semantics.
func TestCostRollups(t *testing.T) {
	s := newTestStore(t)

	now := time.Now()
	// Two sessions in the dev mode, one in ops, one with no mode.
	mkSession := func(id, mode string, age time.Duration) {
		t.Helper()
		err := s.CreateSession(Session{
			ID: id, Goal: "g", Worker: "claude", Status: "completed",
			CreatedAt: now.Add(-age), ModeName: mode,
		})
		if err != nil {
			t.Fatalf("CreateSession(%s): %v", id, err)
		}
	}
	mkSession("sess-dev-a", "dev", 1*time.Hour)
	mkSession("sess-dev-b", "dev", 2*time.Hour)
	mkSession("sess-ops", "ops", 1*time.Hour)
	mkSession("sess-nomode", "", 1*time.Hour)

	mkSubtask := func(id, sessID, worker string, tokensIn, tokensOut, usdCents int64) {
		t.Helper()
		start := now.Add(-30 * time.Minute)
		done := now.Add(-29 * time.Minute)
		if err := s.CreateSubtask(Subtask{
			ID: id, SessionID: sessID, Ord: 0,
			Title: "t", PromptRef: "ref", Worker: worker, Status: "running",
		}); err != nil {
			t.Fatalf("CreateSubtask(%s): %v", id, err)
		}
		meta := `{"tokens_in":` + itoa(tokensIn) + `,"tokens_out":` + itoa(tokensOut) + `,"usd_cents":` + itoa(usdCents) + `}`
		if err := s.UpdateSubtask(Subtask{
			ID: id, Status: "completed",
			StartedAt: &start, CompletedAt: &done,
			MetaJSON: meta,
		}); err != nil {
			t.Fatalf("UpdateSubtask(%s): %v", id, err)
		}
	}
	// dev/claude — two subtasks → 300/180 tokens, 5c total
	mkSubtask("st-1", "sess-dev-a", "claude", 100, 60, 2)
	mkSubtask("st-2", "sess-dev-b", "claude", 200, 120, 3)
	// dev/gemini — single subtask
	mkSubtask("st-3", "sess-dev-a", "gemini", 500, 200, 1)
	// ops/claude
	mkSubtask("st-4", "sess-ops", "claude", 50, 50, 1)
	// no-mode/claude
	mkSubtask("st-5", "sess-nomode", "claude", 1, 1, 0)

	cutoff := now.Add(-24 * time.Hour)
	buckets, err := s.CostRollups(cutoff, "")
	if err != nil {
		t.Fatalf("CostRollups: %v", err)
	}
	// Find (mode=dev, provider=claude). Expect calls=2, tokens_in=300, usd_cents=5.
	var devClaude *CostBucket
	for i := range buckets {
		if buckets[i].ModeName == "dev" && buckets[i].Provider == "claude" {
			devClaude = &buckets[i]
		}
	}
	if devClaude == nil {
		t.Fatalf("missing dev/claude bucket in %+v", buckets)
	}
	if devClaude.Calls != 2 || devClaude.TokensIn != 300 || devClaude.TokensOut != 180 || devClaude.USDCents != 5 {
		t.Errorf("dev/claude rollup wrong: %+v", devClaude)
	}

	// Mode filter "dev" should drop ops + no-mode rows.
	devOnly, err := s.CostRollups(cutoff, "dev")
	if err != nil {
		t.Fatalf("CostRollups(dev): %v", err)
	}
	for _, b := range devOnly {
		if b.ModeName != "dev" {
			t.Errorf("CostRollups(dev) returned non-dev row: %+v", b)
		}
	}
}

func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	sign := ""
	if n < 0 {
		sign = "-"
		n = -n
	}
	digits := ""
	for n > 0 {
		digits = string(rune('0'+(n%10))) + digits
		n /= 10
	}
	return sign + digits
}

func TestCreateSubtask_OrphanFailsFK(t *testing.T) {
	s := newTestStore(t)
	err := s.CreateSubtask(Subtask{
		ID:        "orphan",
		SessionID: "does-not-exist",
		Ord:       0,
		Title:     "x",
		PromptRef: "blob:y",
		Worker:    "claude",
		Status:    "pending",
	})
	if err == nil {
		t.Fatalf("expected FK violation, got nil")
	}
}

// TestResolveSessionID pins the prefix-resolution contract behind every
// CLI surface that accepts the 8-char short ids `uta sessions` prints.
// This was the hello-tour startup bug: step 2 printed short ids that
// step 3 (uta trajectory <id>) rejected.
func TestResolveSessionID(t *testing.T) {
	s := newTestStore(t)
	mk := func(id string) {
		t.Helper()
		if err := s.CreateSession(Session{
			ID: id, Goal: "g", Worker: "w", Status: "completed", CreatedAt: time.Now(),
		}); err != nil {
			t.Fatalf("CreateSession(%s): %v", id, err)
		}
	}
	mk("aabbccdd-1111-2222-3333-444455556666")
	mk("aabbffff-1111-2222-3333-444455556666")
	mk("zz99zz99-1111-2222-3333-444455556666")

	t.Run("exact-match", func(t *testing.T) {
		got, err := s.ResolveSessionID("zz99zz99-1111-2222-3333-444455556666")
		if err != nil || got != "zz99zz99-1111-2222-3333-444455556666" {
			t.Fatalf("got %q err %v", got, err)
		}
	})
	t.Run("unique-prefix", func(t *testing.T) {
		got, err := s.ResolveSessionID("zz99")
		if err != nil || got != "zz99zz99-1111-2222-3333-444455556666" {
			t.Fatalf("got %q err %v", got, err)
		}
	})
	t.Run("short-id-8-chars", func(t *testing.T) {
		got, err := s.ResolveSessionID("aabbccdd")
		if err != nil || got != "aabbccdd-1111-2222-3333-444455556666" {
			t.Fatalf("got %q err %v", got, err)
		}
	})
	t.Run("ambiguous-prefix", func(t *testing.T) {
		_, err := s.ResolveSessionID("aabb")
		if err == nil || !strings.Contains(err.Error(), "ambiguous") {
			t.Fatalf("want ambiguous error, got %v", err)
		}
	})
	t.Run("not-found", func(t *testing.T) {
		_, err := s.ResolveSessionID("ffff")
		if err == nil || !strings.Contains(err.Error(), "not found") {
			t.Fatalf("want not-found error, got %v", err)
		}
	})
	t.Run("empty", func(t *testing.T) {
		if _, err := s.ResolveSessionID("  "); err == nil {
			t.Fatal("want error for empty id")
		}
	})
	t.Run("like-metachar-escaped", func(t *testing.T) {
		// A % in the input must not act as a wildcard and match everything.
		_, err := s.ResolveSessionID("%")
		if err == nil || !strings.Contains(err.Error(), "not found") {
			t.Fatalf("LIKE metachars must be escaped, got %v", err)
		}
	})
}

// TestResolveSessionID_LatestAlias pins the "latest"/"last" aliases: both
// resolve to the most recently created session, and error cleanly on an
// empty store.
func TestResolveSessionID_LatestAlias(t *testing.T) {
	s := newTestStore(t)

	t.Run("empty-store", func(t *testing.T) {
		_, err := s.ResolveSessionID("latest")
		if err == nil || !strings.Contains(err.Error(), "no sessions") {
			t.Fatalf("want no-sessions error, got %v", err)
		}
	})

	base := time.Now().Add(-time.Hour)
	for i, id := range []string{
		"older111-1111-2222-3333-444455556666",
		"newer222-1111-2222-3333-444455556666",
	} {
		if err := s.CreateSession(Session{
			ID: id, Goal: "g", Worker: "w", Status: "completed",
			CreatedAt: base.Add(time.Duration(i) * time.Minute),
		}); err != nil {
			t.Fatalf("CreateSession(%s): %v", id, err)
		}
	}

	for _, alias := range []string{"latest", "last"} {
		got, err := s.ResolveSessionID(alias)
		if err != nil || got != "newer222-1111-2222-3333-444455556666" {
			t.Fatalf("ResolveSessionID(%q) = %q, %v; want newest session", alias, got, err)
		}
	}
}
