package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/unleashtheagents/uta/internal/engine"
	"github.com/unleashtheagents/uta/internal/improve"
	"github.com/unleashtheagents/uta/internal/orgstate"
	"github.com/unleashtheagents/uta/internal/profile"
	"github.com/unleashtheagents/uta/internal/researchdb"
	"github.com/unleashtheagents/uta/internal/store"
)

// TestPanesToRender_EmptyInputs covers the freshly-cloned-repo case: no
// profiles loaded, no sessions recorded → no panes rendered. The output
// must be nil/empty so the dashboard renders cleanly on a brand-new
// project rather than printing four "no data yet" headers.
func TestPanesToRender_EmptyInputs(t *testing.T) {
	got := panesToRender(nil, nil)
	if len(got) != 0 {
		t.Errorf("panesToRender(nil, nil) = %v, want empty slice", got)
	}
	got = panesToRender([]*profile.MissionProfile{}, map[string]*store.ModeStat{})
	if len(got) != 0 {
		t.Errorf("panesToRender(empty, empty) = %v, want empty slice", got)
	}
}

// TestPanesToRender_OnlyProfiles confirms that a loaded profile alone is
// enough to surface its pane — operators see their configured modes even
// before any session has been recorded under that mode.
func TestPanesToRender_OnlyProfiles(t *testing.T) {
	profiles := []*profile.MissionProfile{
		{Name: "dev"},
		{Name: "ops"},
	}
	got := panesToRender(profiles, nil)
	want := []string{"dev", "ops"}
	if !sliceEqual(got, want) {
		t.Errorf("panesToRender = %v, want %v", got, want)
	}
}

// TestPanesToRender_OnlyStats confirms that recorded sessions for any
// mode surface a pane — including user-defined profile names (e.g.
// "marketing") that have no specialized renderer in paneRegistry.
// The unattributed-bucket ("") must still be skipped.
//
// This is the post-Theme-C contract: panesToRender is generic-first.
// Specialized vs. generic-fallback dispatch happens at render time.
func TestPanesToRender_OnlyStats(t *testing.T) {
	stats := map[string]*store.ModeStat{
		"audit":     {TotalSessions: 3},
		"":          {TotalSessions: 1}, // unattributed bucket — must be skipped
		"marketing": {TotalSessions: 7}, // unregistered user mode — must surface (generic fallback)
	}
	got := panesToRender(nil, stats)
	want := []string{"audit", "marketing"}
	if !sliceEqual(got, want) {
		t.Errorf("panesToRender = %v, want %v", got, want)
	}
}

// TestPanesToRender_UnionAndDedup proves the union semantics: a mode
// surfaced by both a profile AND stats appears exactly once.
func TestPanesToRender_UnionAndDedup(t *testing.T) {
	profiles := []*profile.MissionProfile{{Name: "dev"}, {Name: "research"}}
	stats := map[string]*store.ModeStat{
		"dev":   {TotalSessions: 1},
		"audit": {TotalSessions: 2},
	}
	got := panesToRender(profiles, stats)
	want := []string{"audit", "dev", "research"}
	if !sliceEqual(got, want) {
		t.Errorf("panesToRender = %v, want %v", got, want)
	}
}

// TestPanesToRender_NilStatEntry covers the edge case where the stats
// map carries a nil value (defensive — current Store code does not emit
// this, but the helper should tolerate it without panicking).
func TestPanesToRender_NilStatEntry(t *testing.T) {
	stats := map[string]*store.ModeStat{
		"dev":   nil,
		"audit": {TotalSessions: 1},
	}
	got := panesToRender(nil, stats)
	want := []string{"audit"}
	if !sliceEqual(got, want) {
		t.Errorf("panesToRender = %v, want %v", got, want)
	}
}

// TestRegisteredPaneNames_Sorted confirms the helper returns names in a
// stable lexical order — flag-error messages and JSON-key iteration both
// rely on this for diff-stability across runs.
func TestRegisteredPaneNames_Sorted(t *testing.T) {
	got := registeredPaneNames()
	if len(got) == 0 {
		t.Fatal("registeredPaneNames returned empty — expected at least the built-in panes")
	}
	sorted := append([]string(nil), got...)
	sort.Strings(sorted)
	if !sliceEqual(got, sorted) {
		t.Errorf("registeredPaneNames = %v, want sorted %v", got, sorted)
	}
}

// TestEmitDashJSON_SchemaStable_EmptyDB is the snapshot test the audit
// asked for. With no data at all, the JSON top-level shape must still
// contain every registered pane so consumers can rely on a stable schema
// regardless of project state. The keys are pinned to dev/ops/audit/research
// — if a renderer is added or removed, this test should be updated
// deliberately rather than drift silently.
func TestEmitDashJSON_SchemaStable_EmptyDB(t *testing.T) {
	d := &dashData{
		App:      &App{}, // empty App: no ContextDir, nil Store — exercises empty-DB path
		Profiles: nil,
		Stats:    map[string]*store.ModeStat{},
	}
	var buf bytes.Buffer
	if err := emitDashJSON(&buf, d, "", "mode"); err != nil {
		t.Fatalf("emitDashJSON: %v", err)
	}
	var got map[string]json.RawMessage
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v\nraw=%s", err, buf.String())
	}
	wantKeys := []string{"audit", "dev", "ops", "research"}
	gotKeys := make([]string, 0, len(got))
	for k := range got {
		gotKeys = append(gotKeys, k)
	}
	sort.Strings(gotKeys)
	if !sliceEqual(gotKeys, wantKeys) {
		t.Errorf("JSON keys = %v, want %v (schema drift — update this test deliberately)", gotKeys, wantKeys)
	}

	// Per-pane schema: each pane must serialize its "mode" field as the
	// pane name even on empty data — that's the marker downstream tooling
	// uses to verify a pane belongs to the right section.
	for _, key := range wantKeys {
		var pane map[string]any
		if err := json.Unmarshal(got[key], &pane); err != nil {
			t.Errorf("pane %q failed to unmarshal: %v", key, err)
			continue
		}
		if pane["mode"] != key {
			t.Errorf("pane %q .mode = %v, want %q", key, pane["mode"], key)
		}
	}
}

// TestEmitDashJSON_ModeFilter exercises the --mode <name> --json path:
// the output must be a single pane object (not the dashJSON envelope),
// with the matching mode key.
func TestEmitDashJSON_ModeFilter(t *testing.T) {
	d := &dashData{
		App:      &App{},
		Profiles: nil,
		Stats:    map[string]*store.ModeStat{},
	}
	var buf bytes.Buffer
	if err := emitDashJSON(&buf, d, "dev", "mode"); err != nil {
		t.Fatalf("emitDashJSON(dev): %v", err)
	}
	var pane map[string]any
	if err := json.Unmarshal(buf.Bytes(), &pane); err != nil {
		t.Fatalf("unmarshal: %v\nraw=%s", err, buf.String())
	}
	if pane["mode"] != "dev" {
		t.Errorf("filtered pane .mode = %v, want \"dev\"", pane["mode"])
	}
	// The envelope keys MUST NOT appear at the top level when filtered.
	for _, k := range []string{"ops", "audit", "research"} {
		if _, present := pane[k]; present {
			t.Errorf("filtered output unexpectedly contains envelope key %q", k)
		}
	}
}

// TestEmitDashJSON_UnknownModeFilter pins the error path on the JSON
// emitter: a modeFilter that isn't in paneRegistry must surface an
// explicit error rather than silently falling through to the full
// envelope. The CLI flag parser is the primary gate against this, but
// emitDashJSON is a library function that other callers (a future MCP
// server, a TUI) might invoke directly — so the defensive check needs a
// regression guard.
func TestEmitDashJSON_UnknownModeFilter(t *testing.T) {
	d := &dashData{App: &App{}, Stats: map[string]*store.ModeStat{}}
	var buf bytes.Buffer
	err := emitDashJSON(&buf, d, "not-a-real-mode", "mode")
	if err == nil {
		t.Fatal("emitDashJSON(unknown mode) returned nil error; want a descriptive error")
	}
	if !strings.Contains(err.Error(), "not-a-real-mode") {
		t.Errorf("error %q does not mention the offending mode name", err.Error())
	}
	if buf.Len() != 0 {
		t.Errorf("emitDashJSON wrote %q on error; want no partial output", buf.String())
	}
}

// TestRenderDashPane_EmptyDataIsResilient asserts the registered pane
// renderers don't panic or error on a freshly initialized dashData
// (no App context, no stats). This is the audit's "exercise with a fresh
// empty DB" check distilled to the renderer surface — the dash command
// must remain useful on day 1 of a project.
func TestRenderDashPane_EmptyDataIsResilient(t *testing.T) {
	d := &dashData{App: &App{}, Stats: map[string]*store.ModeStat{}}
	for _, name := range registeredPaneNames() {
		var buf bytes.Buffer
		renderDashPane(&buf, d, name, "mode")
		if buf.Len() == 0 {
			t.Errorf("pane %q rendered nothing on empty data", name)
			continue
		}
		// The pane header convention is "== <Mode> mode pane ==" — case
		// matters less than the section marker.
		first := strings.SplitN(buf.String(), "\n", 2)[0]
		if !strings.Contains(strings.ToLower(first), name) {
			t.Errorf("pane %q first line %q does not mention the mode name", name, first)
		}
	}
}

// TestRenderDashPane_UnregisteredFallsBackToGeneric is the post-Theme-C
// contract: a mode name with no specialized pane in paneRegistry must
// still render — using the generic ideas+sessions view, with the mode
// name in the header. Empty name remains a no-op (defensive).
func TestRenderDashPane_UnregisteredFallsBackToGeneric(t *testing.T) {
	d := &dashData{App: &App{}, Stats: map[string]*store.ModeStat{}}
	var buf bytes.Buffer
	renderDashPane(&buf, d, "marketing", "mode")
	got := buf.String()
	if got == "" {
		t.Fatal("unregistered mode should render via generic fallback, got empty")
	}
	if !strings.Contains(got, "Marketing mode pane") {
		t.Errorf("generic fallback should title-case the mode name: %q", got)
	}
	if !strings.Contains(got, "mode:marketing") {
		t.Errorf("idea-tag hint should use mode:<name> shape: %q", got)
	}

	// Empty name must still be a no-op so accidental calls don't pollute.
	var emptyBuf bytes.Buffer
	renderDashPane(&emptyBuf, d, "", "mode")
	if emptyBuf.Len() != 0 {
		t.Errorf("empty mode name should be a no-op, got %q", emptyBuf.String())
	}
}

// TestBuildAuditPane_PopulatedHeatmap is the audit's "verify aggregation"
// pass: drop a synthetic findings.json into a temp ContextDir, build the
// audit pane, and confirm the severity-by-file heatmap counts match the
// raw findings. A populated case is the only way to catch off-by-one or
// keying bugs — the empty-data tests above only cover the no-data branch.
func TestBuildAuditPane_PopulatedHeatmap(t *testing.T) {
	ctxDir := t.TempDir()
	rep := engine.FindingsReport{
		Iteration:  3,
		ProducedAt: time.Date(2026, 5, 18, 12, 0, 0, 0, time.UTC),
		Findings: []engine.Finding{
			{ID: "1", Severity: engine.SevHigh, File: "a.go", Title: "x"},
			{ID: "2", Severity: engine.SevHigh, File: "a.go", Title: "y"},
			{ID: "3", Severity: engine.SevMedium, File: "a.go", Title: "z"},
			{ID: "4", Severity: engine.SevLow, File: "b.go", Title: "q"},
			{ID: "5", Severity: "", File: "", Title: "no-file no-sev"}, // unknown + (no-file) bucket
		},
	}
	raw, err := json.Marshal(rep)
	if err != nil {
		t.Fatalf("marshal report: %v", err)
	}
	if err := os.WriteFile(filepath.Join(ctxDir, "findings.json"), raw, 0o644); err != nil {
		t.Fatalf("write findings.json: %v", err)
	}

	d := &dashData{
		App:   &App{ContextDir: ctxDir, ProjectRoot: ctxDir}, // ProjectRoot non-empty → InProject() true
		Stats: map[string]*store.ModeStat{},
	}
	pane := buildAuditPane(d)

	if !pane.FindingsExist {
		t.Fatal("FindingsExist = false; want true after writing findings.json")
	}
	if pane.TotalFindings != 5 {
		t.Errorf("TotalFindings = %d, want 5", pane.TotalFindings)
	}
	if pane.Iteration != 3 {
		t.Errorf("Iteration = %d, want 3", pane.Iteration)
	}
	if got := pane.BySeverity[string(engine.SevHigh)]; got != 2 {
		t.Errorf("BySeverity[high] = %d, want 2", got)
	}
	if got := pane.BySeverity[string(engine.SevUnknown)]; got != 1 {
		t.Errorf("BySeverity[unknown] = %d, want 1 (empty severity normalizes to unknown)", got)
	}
	if got := pane.ByFile["a.go"]; got != 3 {
		t.Errorf("ByFile[a.go] = %d, want 3", got)
	}
	if got := pane.ByFile["(no-file)"]; got != 1 {
		t.Errorf("ByFile[(no-file)] = %d, want 1 (empty file path bucketed as (no-file))", got)
	}
	if got := pane.Heatmap["a.go"][string(engine.SevHigh)]; got != 2 {
		t.Errorf("Heatmap[a.go][high] = %d, want 2", got)
	}
	if got := pane.Heatmap["b.go"][string(engine.SevLow)]; got != 1 {
		t.Errorf("Heatmap[b.go][low] = %d, want 1", got)
	}
	wantHighFiles := []string{"a.go", "a.go"}
	if !sliceEqual(pane.HighSeverityFiles, wantHighFiles) {
		t.Errorf("HighSeverityFiles = %v, want %v (one entry per high finding)",
			pane.HighSeverityFiles, wantHighFiles)
	}
}

// TestBuildAuditPane_MalformedFindings asserts a malformed findings.json
// falls back to the "no findings yet" state rather than crashing the
// dashboard. The audit's grace-on-bad-data requirement applies equally
// to artifacts an operator might have hand-edited.
func TestBuildAuditPane_MalformedFindings(t *testing.T) {
	ctxDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(ctxDir, "findings.json"), []byte("{not valid json"), 0o644); err != nil {
		t.Fatalf("write findings.json: %v", err)
	}
	d := &dashData{
		App:   &App{ContextDir: ctxDir, ProjectRoot: ctxDir},
		Stats: map[string]*store.ModeStat{},
	}
	pane := buildAuditPane(d)
	if pane.FindingsExist {
		t.Error("FindingsExist = true on malformed JSON; want false")
	}
	if pane.TotalFindings != 0 {
		t.Errorf("TotalFindings = %d, want 0 on malformed JSON", pane.TotalFindings)
	}
}

// TestBuildOpsPane_NotInProject covers the global-home invocation path:
// no ContextDir means no ORG_STATE.md path can be resolved, so the pane
// must report the empty/missing state without panicking or touching the
// filesystem.
func TestBuildOpsPane_NotInProject(t *testing.T) {
	d := &dashData{App: &App{}, Stats: map[string]*store.ModeStat{}}
	pane := buildOpsPane(d)
	if pane.Mode != "ops" {
		t.Errorf("Mode = %q, want \"ops\"", pane.Mode)
	}
	if pane.OrgStatePath != "" {
		t.Errorf("OrgStatePath = %q, want empty when ContextDir is unset", pane.OrgStatePath)
	}
	if pane.OrgStateExists {
		t.Error("OrgStateExists = true, want false when ContextDir is unset")
	}
}

// TestBuildOpsPane_OrgStateMissing covers the in-project case before
// the first comms-agent write: ContextDir is set so the path resolves,
// but the file itself has not been created yet. The pane must reflect
// "path known, file absent" so the dash text view can print its
// "ORG_STATE.md: (not created yet)" affordance.
func TestBuildOpsPane_OrgStateMissing(t *testing.T) {
	ctxDir := t.TempDir()
	d := &dashData{
		App:   &App{ContextDir: ctxDir, ProjectRoot: ctxDir},
		Stats: map[string]*store.ModeStat{},
	}
	pane := buildOpsPane(d)
	if pane.OrgStatePath != orgstate.Path(ctxDir) {
		t.Errorf("OrgStatePath = %q, want %q", pane.OrgStatePath, orgstate.Path(ctxDir))
	}
	if pane.OrgStateExists {
		t.Error("OrgStateExists = true with no ORG_STATE.md on disk")
	}
	if pane.SourceEmails != 0 || pane.PendingDecisions != 0 {
		t.Errorf("counts = (%d, %d), want (0, 0) when file is absent",
			pane.SourceEmails, pane.PendingDecisions)
	}
}

// TestBuildOpsPane_OrgStatePopulated is the integration test for the
// orgstate -> dash path. Write a real ORG_STATE.md via the orgstate
// package, then assert buildOpsPane surfaces the frontmatter counts and
// timestamp. This is the regression guard for "someone refactors away
// the orgstate.Load call and the dash silently shows zero forever".
func TestBuildOpsPane_OrgStatePopulated(t *testing.T) {
	ctxDir := t.TempDir()
	stamp := time.Date(2026, 5, 18, 9, 30, 0, 0, time.UTC)
	st := &orgstate.State{
		Frontmatter: orgstate.Frontmatter{
			LastUpdated:      stamp,
			SourceEmails:     []string{"msg-a", "msg-b", "msg-c"},
			PendingDecisions: []string{"approve Acme MSA"},
		},
		Body: "## Commitments\n- ship v1\n",
	}
	if err := orgstate.Write(ctxDir, st); err != nil {
		t.Fatalf("orgstate.Write: %v", err)
	}
	d := &dashData{
		App:   &App{ContextDir: ctxDir, ProjectRoot: ctxDir},
		Stats: map[string]*store.ModeStat{},
	}
	pane := buildOpsPane(d)
	if !pane.OrgStateExists {
		t.Fatal("OrgStateExists = false; want true after orgstate.Write")
	}
	if pane.SourceEmails != 3 {
		t.Errorf("SourceEmails = %d, want 3", pane.SourceEmails)
	}
	if pane.PendingDecisions != 1 {
		t.Errorf("PendingDecisions = %d, want 1", pane.PendingDecisions)
	}
	if pane.OrgStateLastSet == nil || !pane.OrgStateLastSet.Equal(stamp) {
		t.Errorf("OrgStateLastSet = %v, want %v", pane.OrgStateLastSet, stamp)
	}
	if pane.OrgStateBytes <= 0 {
		t.Errorf("OrgStateBytes = %d, want > 0", pane.OrgStateBytes)
	}
}

// TestBuildOpsPane_MalformedOrgState pins the grace-on-bad-data contract:
// an ORG_STATE.md with broken YAML frontmatter must not crash the dash.
// The file is present on disk (so OrgStateExists stays true and bytes
// are reported), but the structured counts fall back to zero rather
// than panicking or leaking the parse error to the caller.
func TestBuildOpsPane_MalformedOrgState(t *testing.T) {
	ctxDir := t.TempDir()
	bad := "---\nsource_emails: 42\n---\nbody\n" // scalar where []string is required
	if err := os.WriteFile(orgstate.Path(ctxDir), []byte(bad), 0o644); err != nil {
		t.Fatalf("write ORG_STATE.md: %v", err)
	}
	d := &dashData{
		App:   &App{ContextDir: ctxDir, ProjectRoot: ctxDir},
		Stats: map[string]*store.ModeStat{},
	}
	pane := buildOpsPane(d)
	if !pane.OrgStateExists {
		t.Error("OrgStateExists = false; want true (the file is on disk even if unparseable)")
	}
	if pane.OrgStateBytes == 0 {
		t.Error("OrgStateBytes = 0; want > 0 for a non-empty file")
	}
	if pane.SourceEmails != 0 || pane.PendingDecisions != 0 {
		t.Errorf("counts = (%d, %d), want (0, 0) on malformed frontmatter",
			pane.SourceEmails, pane.PendingDecisions)
	}
	if pane.OrgStateLastSet != nil {
		t.Errorf("OrgStateLastSet = %v, want nil on malformed frontmatter", pane.OrgStateLastSet)
	}
}

// TestCountOpsDrafts_DedupesAndFilters proves countOpsDrafts counts each
// logical draft creation exactly once and ignores tool calls from other
// modes. The previous implementation matched IN ('subtask_tool_call',
// 'tool_started', 'tool_completed') and used a substring payload LIKE,
// which both multi-counted (started+completed pair) and could over-match
// (a list_drafts call with a query containing "create_draft" would slip
// through). The current implementation extracts $.name from the JSON
// payload and restricts to one kind-per-call.
func TestCountOpsDrafts_DedupesAndFilters(t *testing.T) {
	s, err := store.Open(filepath.Join(t.TempDir(), "ops.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer s.Close()

	mustSession := func(id, mode string) {
		t.Helper()
		if err := s.CreateSession(store.Session{
			ID: id, Goal: "g", Status: "completed",
			CreatedAt: time.Now(), ModeName: mode,
		}); err != nil {
			t.Fatalf("CreateSession(%s): %v", id, err)
		}
	}
	mustEvent := func(sessID string, seq int64, kind, name string) {
		t.Helper()
		payload, _ := json.Marshal(map[string]any{"name": name, "input": map[string]string{"to": "x@y"}})
		if err := s.InsertEvent(sessID, "", seq, time.Now(), kind, payload); err != nil {
			t.Fatalf("InsertEvent: %v", err)
		}
	}

	mustSession("ops1", "ops")
	mustSession("dev1", "dev")

	// One real Gmail draft from the ops worker (subtask_tool_call only — that's
	// the kind worker tool calls emit).
	mustEvent("ops1", 1, "subtask_tool_call", "mcp__claude_ai_Gmail__create_draft")

	// A Reflector-style emission (tool_started + tool_completed pair) for a
	// hypothetical future ops Reflector run. We expect EXACTLY ONE count here,
	// not two — the query should match tool_completed but skip tool_started.
	mustEvent("ops1", 2, "tool_started", "mcp__claude_ai_Gmail__create_draft")
	mustEvent("ops1", 3, "tool_completed", "mcp__claude_ai_Gmail__create_draft")

	// A list_drafts call whose argument text contains the substring
	// "create_draft" — the OLD LIKE-on-payload query would falsely include
	// this; the json_extract($.name) query must NOT.
	listPayload, _ := json.Marshal(map[string]any{
		"name":  "mcp__claude_ai_Gmail__list_drafts",
		"input": map[string]string{"query": "search for create_draft references"},
	})
	if err := s.InsertEvent("ops1", "", 4, time.Now(), "subtask_tool_call", listPayload); err != nil {
		t.Fatalf("InsertEvent(list_drafts): %v", err)
	}

	// A dev-mode draft attempt — must be excluded by the mode_name filter.
	mustEvent("dev1", 1, "subtask_tool_call", "mcp__claude_ai_Gmail__create_draft")

	app := &App{Store: s}
	got := countOpsDrafts(app)
	if got != 2 {
		t.Errorf("countOpsDrafts = %d, want 2 (one worker call + one Reflector completion; substring-only payloads must be ignored)", got)
	}
}

// TestCountOpsDrafts_NilSafe covers the brand-new-project path: App with
// no Store must not panic and must return 0.
func TestCountOpsDrafts_NilSafe(t *testing.T) {
	if got := countOpsDrafts(nil); got != 0 {
		t.Errorf("countOpsDrafts(nil) = %d, want 0", got)
	}
	if got := countOpsDrafts(&App{}); got != 0 {
		t.Errorf("countOpsDrafts(&App{}) = %d, want 0", got)
	}
	if got := countDraftsForMode(&App{}, "ops"); got != 0 {
		t.Errorf("countDraftsForMode(&App{}, ops) = %d, want 0", got)
	}
	if got := countDraftsForMode(nil, "ops"); got != 0 {
		t.Errorf("countDraftsForMode(nil, ops) = %d, want 0", got)
	}
}

// TestCountDraftsForMode_GenericModeName proves the SQL no longer hardcodes
// 'ops' as the mode filter — a user-defined profile named "publishing"
// (or anything else) gets the same draft count when its sessions ran
// the equivalent tool. This is the Theme C audit's specific gap on
// dash.go:792 closed.
func TestCountDraftsForMode_GenericModeName(t *testing.T) {
	s, err := store.Open(filepath.Join(t.TempDir(), "modes.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer s.Close()

	mustSession := func(id, mode string) {
		t.Helper()
		if err := s.CreateSession(store.Session{
			ID: id, Goal: "g", Status: "completed",
			CreatedAt: time.Now(), ModeName: mode,
		}); err != nil {
			t.Fatalf("CreateSession: %v", err)
		}
	}
	mustEvent := func(sessID string, seq int64, name string) {
		t.Helper()
		payload, _ := json.Marshal(map[string]any{"name": name})
		if err := s.InsertEvent(sessID, "", seq, time.Now(), "subtask_tool_call", payload); err != nil {
			t.Fatalf("InsertEvent: %v", err)
		}
	}

	mustSession("publishing1", "publishing")
	mustSession("publishing2", "publishing")
	mustSession("comms1", "comms")
	mustSession("ops1", "ops")

	mustEvent("publishing1", 1, "mcp__claude_ai_Gmail__create_draft")
	mustEvent("publishing2", 1, "mcp__claude_ai_WordPress__create_draft")
	mustEvent("comms1", 1, "mcp__claude_ai_Ghost__create_draft")
	mustEvent("ops1", 1, "mcp__claude_ai_Gmail__create_draft")

	app := &App{Store: s}
	cases := map[string]int{
		"publishing":  2, // both publishing sessions count
		"comms":       1,
		"ops":         1, // backward-compat — old caller still works
		"nonexistent": 0,
	}
	for mode, want := range cases {
		if got := countDraftsForMode(app, mode); got != want {
			t.Errorf("countDraftsForMode(%q) = %d, want %d", mode, got, want)
		}
	}

	// Empty mode name must not return a phantom count of all drafts.
	if got := countDraftsForMode(app, ""); got != 0 {
		t.Errorf("countDraftsForMode(empty) = %d, want 0 (mode-required guard)", got)
	}
}

// TestBuildDevPane_PopulatedCounts is the dev pane's populated-data
// regression: a mix of ideas (some mode:dev, some mode:audit, some
// untagged) and a synthetic stat row with sessions of varied statuses.
// Asserts the per-status idea counts, the done-ratio, the session
// status histogram, and the derived test_pass_rate all aggregate
// correctly. Empty-data paths are covered by the resilience test; this
// is the off-by-one / wrong-key guard.
func TestBuildDevPane_PopulatedCounts(t *testing.T) {
	d := &dashData{
		App: &App{},
		Ideas: []*improve.Idea{
			{ID: "a", Title: "x", Status: improve.StatusDone, Tags: []string{"mode:dev"}},
			{ID: "b", Title: "y", Status: improve.StatusDone, Tags: []string{"Mode:DEV"}}, // case-insensitive match
			{ID: "c", Title: "z", Status: improve.StatusProposed, Tags: []string{"mode:dev"}},
			{ID: "d", Title: "q", Status: improve.StatusFailed, Tags: []string{"mode:audit"}}, // wrong mode
			{ID: "e", Title: "r", Status: improve.StatusDone, Tags: nil},                      // untagged
		},
		Stats: map[string]*store.ModeStat{
			"dev": {
				TotalSessions: 4,
				RecentSessions: []store.Session{
					{ID: "s1", Status: "completed"},
					{ID: "s2", Status: "completed"},
					{ID: "s3", Status: "failed"},
					{ID: "s4", Status: "running"}, // excluded from pass-rate denominator
				},
			},
		},
	}
	p := buildDevPane(d, "mode")

	if p.Mode != "dev" {
		t.Errorf("Mode = %q, want \"dev\"", p.Mode)
	}
	if p.IdeasTotal != 3 {
		t.Errorf("IdeasTotal = %d, want 3 (2 dev-done + 1 dev-proposed; audit and untagged excluded)", p.IdeasTotal)
	}
	if got := p.IdeasByStatus[improve.StatusDone]; got != 2 {
		t.Errorf("IdeasByStatus[done] = %d, want 2", got)
	}
	if got := p.IdeasByStatus[improve.StatusProposed]; got != 1 {
		t.Errorf("IdeasByStatus[proposed] = %d, want 1", got)
	}
	if got := p.IdeasByStatus[improve.StatusFailed]; got != 0 {
		t.Errorf("IdeasByStatus[failed] = %d, want 0 (audit-mode idea must not leak in)", got)
	}
	if want := 2.0 / 3.0; p.IdeasDoneRatio != want {
		t.Errorf("IdeasDoneRatio = %v, want %v", p.IdeasDoneRatio, want)
	}
	if p.SessionsTotal != 4 {
		t.Errorf("SessionsTotal = %d, want 4", p.SessionsTotal)
	}
	if got := p.SessionsByStatus["completed"]; got != 2 {
		t.Errorf("SessionsByStatus[completed] = %d, want 2", got)
	}
	if got := p.SessionsByStatus["running"]; got != 1 {
		t.Errorf("SessionsByStatus[running] = %d, want 1", got)
	}
	// passAndTotal: 2 completed of 3 terminal (completed+completed+failed; running excluded) → 2/3.
	if want := 2.0 / 3.0; p.TestPassRate != want {
		t.Errorf("TestPassRate = %v, want %v (running excluded from denominator)", p.TestPassRate, want)
	}
}

// TestBuildDevPane_CustomTagKey proves --idea-tag-key is wired into the
// pane builder: ideas tagged with "topic:dev" must be picked up when
// the caller asks for tagKey="topic", and the default "mode" prefix
// must NOT spuriously match.
func TestBuildDevPane_CustomTagKey(t *testing.T) {
	d := &dashData{
		App:   &App{},
		Stats: map[string]*store.ModeStat{},
		Ideas: []*improve.Idea{
			{ID: "a", Title: "x", Status: improve.StatusDone, Tags: []string{"topic:dev"}},
			{ID: "b", Title: "y", Status: improve.StatusDone, Tags: []string{"mode:dev"}},
		},
	}
	pTopic := buildDevPane(d, "topic")
	if pTopic.IdeasTotal != 1 {
		t.Errorf("topic-keyed pane IdeasTotal = %d, want 1 (only topic:dev should match)", pTopic.IdeasTotal)
	}
	pMode := buildDevPane(d, "mode")
	if pMode.IdeasTotal != 1 {
		t.Errorf("mode-keyed pane IdeasTotal = %d, want 1 (only mode:dev should match)", pMode.IdeasTotal)
	}
}

// TestBuildResearchPane_Populated writes a real RESEARCH_DATABASE.md
// via researchdb.Write, then asserts buildResearchPane surfaces the
// frontmatter counts and the derived claims-per-source ratio. One
// claim is intentionally left uncited to exercise the UncitedClaims
// branch — and since researchdb.Write rejects uncited claims, we have
// to write that variant directly without going through the validator.
func TestBuildResearchPane_Populated(t *testing.T) {
	ctxDir := t.TempDir()
	stamp := time.Date(2026, 5, 18, 10, 0, 0, 0, time.UTC)
	st := &researchdb.State{
		Frontmatter: researchdb.Frontmatter{
			LastUpdated: stamp,
			Queries:     []string{"q1", "q2"},
			Sources: []researchdb.Source{
				{URL: "https://a.example", Retrieved: stamp},
				{URL: "https://b.example", Retrieved: stamp},
			},
			Claims: []researchdb.Claim{
				{Text: "c1", Sources: []int{1}},
				{Text: "c2", Sources: []int{1, 2}},
				{Text: "c3", Sources: []int{2}},
			},
		},
		Body: "## Notes\n",
	}
	if err := researchdb.Write(ctxDir, st); err != nil {
		t.Fatalf("researchdb.Write: %v", err)
	}
	d := &dashData{
		App:   &App{ContextDir: ctxDir, ProjectRoot: ctxDir},
		Stats: map[string]*store.ModeStat{"research": {TotalSessions: 7}},
	}
	pane := buildResearchPane(d)
	if pane.Mode != "research" {
		t.Errorf("Mode = %q, want \"research\"", pane.Mode)
	}
	if !pane.DBExists {
		t.Fatal("DBExists = false; want true after researchdb.Write")
	}
	if pane.Queries != 2 {
		t.Errorf("Queries = %d, want 2", pane.Queries)
	}
	if pane.Sources != 2 {
		t.Errorf("Sources = %d, want 2", pane.Sources)
	}
	if pane.Claims != 3 {
		t.Errorf("Claims = %d, want 3", pane.Claims)
	}
	if want := 3.0 / 2.0; pane.ClaimsPerSource != want {
		t.Errorf("ClaimsPerSource = %v, want %v", pane.ClaimsPerSource, want)
	}
	if pane.SessionsTotal != 7 {
		t.Errorf("SessionsTotal = %d, want 7", pane.SessionsTotal)
	}
	if pane.LastUpdated == nil || !pane.LastUpdated.Equal(stamp) {
		t.Errorf("LastUpdated = %v, want %v", pane.LastUpdated, stamp)
	}
	if pane.BytesOnDisk <= 0 {
		t.Errorf("BytesOnDisk = %d, want > 0", pane.BytesOnDisk)
	}
	if pane.UncitedClaims != 0 {
		t.Errorf("UncitedClaims = %d, want 0 (every claim cites a source)", pane.UncitedClaims)
	}
}

// TestBuildResearchPane_UncitedClaims pins the uncited-claim branch by
// writing a RESEARCH_DATABASE.md directly (bypassing the validator
// that would normally reject this). The dash should still surface the
// uncited count rather than crash — an operator who hand-edits the
// ledger past the validator should see the breakage on the dashboard.
func TestBuildResearchPane_UncitedClaims(t *testing.T) {
	ctxDir := t.TempDir()
	raw := `---
last_updated: 2026-05-18T10:00:00Z
sources:
  - url: https://a.example
    retrieved: 2026-05-18T10:00:00Z
claims:
  - text: cited claim
    sources: [1]
  - text: orphan claim
    sources: []
---
body
`
	if err := os.WriteFile(researchdb.Path(ctxDir), []byte(raw), 0o644); err != nil {
		t.Fatalf("write RESEARCH_DATABASE.md: %v", err)
	}
	d := &dashData{App: &App{ContextDir: ctxDir, ProjectRoot: ctxDir}, Stats: map[string]*store.ModeStat{}}
	pane := buildResearchPane(d)
	if !pane.DBExists {
		t.Fatal("DBExists = false; want true")
	}
	if pane.Claims != 2 {
		t.Errorf("Claims = %d, want 2", pane.Claims)
	}
	if pane.UncitedClaims != 1 {
		t.Errorf("UncitedClaims = %d, want 1", pane.UncitedClaims)
	}
}

// TestBuildGenericPane_AnyModeName proves the prototypical pane is
// generic-first: pass any mode name, the helper reads ideas tagged
// "mode:<name>" and sessions under d.Stats[name]. This is the
// Theme C audit's "no specialized pane" fallback path.
func TestBuildGenericPane_AnyModeName(t *testing.T) {
	ideas := []*improve.Idea{
		{ID: "i1", Status: "done", Tags: []string{"mode:growth"}},
		{ID: "i2", Status: "proposed", Tags: []string{"mode:growth"}},
		{ID: "i3", Status: "done", Tags: []string{"mode:dev"}},
	}
	d := &dashData{
		App:   &App{},
		Ideas: ideas,
		Stats: map[string]*store.ModeStat{
			"growth": {TotalSessions: 4},
		},
	}
	pane := buildGenericPane(d, "mode", "growth")

	if pane.Mode != "growth" {
		t.Errorf("Mode = %q, want growth", pane.Mode)
	}
	if pane.IdeasTotal != 2 {
		t.Errorf("IdeasTotal = %d, want 2 (only growth-tagged)", pane.IdeasTotal)
	}
	if pane.IdeasByStatus["done"] != 1 {
		t.Errorf("done = %d, want 1", pane.IdeasByStatus["done"])
	}
	if pane.SessionsTotal != 4 {
		t.Errorf("SessionsTotal = %d, want 4", pane.SessionsTotal)
	}
}

// TestBuildOpsPaneFor_GenericModeName proves the ops pane works with any
// mode name — a profile named "publishing" or "comms" gets the same
// ORG_STATE.md + drafts view as one named "ops". Closes the audit's
// "buildOpsPane hardcodes 'ops'" gap.
func TestBuildOpsPaneFor_GenericModeName(t *testing.T) {
	d := &dashData{
		App: &App{},
		Stats: map[string]*store.ModeStat{
			"publishing": {TotalSessions: 9},
			"ops":        {TotalSessions: 2},
		},
	}
	publishing := buildOpsPaneFor(d, "publishing")
	if publishing.Mode != "publishing" {
		t.Errorf("Mode = %q, want publishing", publishing.Mode)
	}
	if publishing.SessionsTotal != 9 {
		t.Errorf("SessionsTotal under custom name = %d, want 9", publishing.SessionsTotal)
	}

	ops := buildOpsPaneFor(d, "ops")
	if ops.SessionsTotal != 2 {
		t.Errorf("backward-compat ops mode broken: SessionsTotal = %d, want 2", ops.SessionsTotal)
	}
}

func sliceEqual(a, b []string) bool {
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
