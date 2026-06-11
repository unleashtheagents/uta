package engine

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/unleashtheagents/uta/internal/memory"
	"github.com/unleashtheagents/uta/internal/provider"
	"github.com/unleashtheagents/uta/internal/store"
	"github.com/unleashtheagents/uta/internal/trajectory"
)

// memTestSetup wires a Deps that has a real institutional_facts-backed
// memory.Store, an event-bus subscription drained into a slice for
// inspection, and a persisted session row so emit() doesn't drop events
// at the recorder due to the trajectory_events foreign key.
type memTestSetup struct {
	t         *testing.T
	deps      Deps
	sup       *Supervisor
	sessionID string
	events    <-chan trajectory.Event
}

func newMemTestSetup(t *testing.T) *memTestSetup {
	t.Helper()
	deps := newTestDeps(t)
	deps.Memory = memory.New(deps.Store.DB)

	sessionID := "sess-mem-test"
	if err := deps.Store.CreateSession(store.Session{
		ID:        sessionID,
		Goal:      "test goal",
		Worker:    "worker",
		Status:    "running",
		CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	ch := deps.Bus.Subscribe(64)
	return &memTestSetup{
		t:         t,
		deps:      deps,
		sup:       New(deps),
		sessionID: sessionID,
		events:    ch,
	}
}

// drainEvents pulls every event currently on the bus subscription without
// blocking. Each emit() above publishes synchronously to a buffered
// channel so by the time the test returns from the call, the events are
// already there.
func (m *memTestSetup) drainEvents() []trajectory.Event {
	var out []trajectory.Event
	for {
		select {
		case ev := <-m.events:
			out = append(out, ev)
		default:
			return out
		}
	}
}

func eventsOfKind(events []trajectory.Event, kind trajectory.Kind) []trajectory.Event {
	var out []trajectory.Event
	for _, ev := range events {
		if ev.Kind == kind {
			out = append(out, ev)
		}
	}
	return out
}

func decodePayload(t *testing.T, ev trajectory.Event) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(ev.Payload, &m); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	return m
}

// allFacts pulls every persisted fact out of the test database. Used to
// assert both the count and the content of writes performed by the
// hooks under test.
func allFacts(t *testing.T, s *store.Store) []memory.Fact {
	t.Helper()
	rows, err := s.DB.Query(`SELECT id, COALESCE(source_mode, ''), COALESCE(source_session_id, ''), kind, body, created_at, tags
                               FROM institutional_facts ORDER BY id`)
	if err != nil {
		t.Fatalf("query facts: %v", err)
	}
	defer rows.Close()
	var out []memory.Fact
	for rows.Next() {
		var f memory.Fact
		var createdNs int64
		var tagsStr string
		if err := rows.Scan(&f.ID, &f.SourceMode, &f.SourceSessionID, &f.Kind, &f.Body, &createdNs, &tagsStr); err != nil {
			t.Fatalf("scan fact: %v", err)
		}
		f.CreatedAt = time.Unix(0, createdNs)
		if tagsStr != "" {
			f.Tags = strings.Fields(tagsStr)
		}
		out = append(out, f)
	}
	return out
}

func TestWriteHighFindingsToMemory_NoMemoryDep(t *testing.T) {
	deps := newTestDeps(t)
	// deps.Memory left nil intentionally.
	sup := New(deps)
	// Must not panic. report can be non-nil; the early return on nil
	// Memory is the contract we're asserting.
	sup.writeHighFindingsToMemory("sess", "dev", &FindingsReport{
		Findings: []Finding{{Severity: SevHigh, Title: "x"}},
	})
}

func TestWriteHighFindingsToMemory_NilReport(t *testing.T) {
	m := newMemTestSetup(t)
	m.sup.writeHighFindingsToMemory(m.sessionID, "dev", nil)
	if got := allFacts(t, m.deps.Store); len(got) != 0 {
		t.Fatalf("expected no facts on nil report, got %+v", got)
	}
}

func TestWriteHighFindingsToMemory_SkipsNonHighSeverity(t *testing.T) {
	m := newMemTestSetup(t)
	m.sup.writeHighFindingsToMemory(m.sessionID, "dev", &FindingsReport{
		Findings: []Finding{
			{Critic: "c", ID: "1", Severity: SevMedium, Title: "med thing"},
			{Critic: "c", ID: "2", Severity: SevLow, Title: "low thing"},
			{Critic: "c", ID: "3", Severity: SevInfo, Title: "info thing"},
		},
	})
	facts := allFacts(t, m.deps.Store)
	if len(facts) != 0 {
		t.Fatalf("non-high findings should not write facts, got %+v", facts)
	}
	if got := eventsOfKind(m.drainEvents(), trajectory.MemoryFactWritten); len(got) != 0 {
		t.Fatalf("expected no MemoryFactWritten events, got %d", len(got))
	}
}

func TestWriteHighFindingsToMemory_WritesAndEmitsForHigh(t *testing.T) {
	m := newMemTestSetup(t)
	m.sup.writeHighFindingsToMemory(m.sessionID, "audit", &FindingsReport{
		Findings: []Finding{
			{Critic: "sol-critic", ID: "h1", Severity: SevHigh, Title: "Reentrancy in withdraw", Body: "withdraw() calls external before state update", File: "contracts/Vault.sol", Line: 42},
			{Critic: "sol-critic", ID: "m1", Severity: SevMedium, Title: "naming nit"},
			{Critic: "sol-critic", ID: "h2", Severity: SevHigh, Title: "Missing access control"}, // body empty -> falls back to title
		},
	})

	facts := allFacts(t, m.deps.Store)
	if len(facts) != 2 {
		t.Fatalf("expected 2 high-finding facts, got %d: %+v", len(facts), facts)
	}

	// First fact: full title+body composition.
	first := facts[0]
	if first.Kind != "lint_rule" {
		t.Fatalf("fact[0] Kind: want lint_rule, got %q", first.Kind)
	}
	if first.SourceMode != "audit" {
		t.Fatalf("fact[0] SourceMode: want audit, got %q", first.SourceMode)
	}
	if first.SourceSessionID != m.sessionID {
		t.Fatalf("fact[0] SourceSessionID: want %q, got %q", m.sessionID, first.SourceSessionID)
	}
	if !strings.Contains(first.Body, "Reentrancy in withdraw") {
		t.Fatalf("fact[0] body missing title: %q", first.Body)
	}
	if !strings.Contains(first.Body, "withdraw() calls external before state update") {
		t.Fatalf("fact[0] body missing finding body: %q", first.Body)
	}
	// Tags should include severity, source mode, critic, file ext, basename, and tokenized title.
	// Tags are normalized (split on non-alphanumeric, lowercased, deduped),
	// so "Vault.sol" becomes "vault" + "sol", "Reentrancy in withdraw"
	// becomes "reentrancy" + "withdraw" (plus the "in" stop-word stripped
	// to a 2-char token).
	tagSet := map[string]bool{}
	for _, tag := range first.Tags {
		tagSet[tag] = true
	}
	for _, want := range []string{"high", "audit", "sol", "vault", "reentrancy", "withdraw", "critic", "finding"} {
		if !tagSet[want] {
			t.Fatalf("fact[0] tags missing %q: %v", want, first.Tags)
		}
	}

	// Second fact: body falls back to title when Body is empty.
	second := facts[1]
	if !strings.Contains(second.Body, "Missing access control") {
		t.Fatalf("fact[1] body should fall back to title, got %q", second.Body)
	}
	// "Title\n\nTitle" since body==title means the composed string has the
	// title twice — the present hook is intentional and we lock it in here.
	if strings.Count(second.Body, "Missing access control") != 2 {
		t.Fatalf("fact[1] should contain title twice when body empty (title + fallback body), got %q", second.Body)
	}

	// Trajectory: one MemoryFactWritten per high finding, both source=high_finding.
	written := eventsOfKind(m.drainEvents(), trajectory.MemoryFactWritten)
	if len(written) != 2 {
		t.Fatalf("expected 2 MemoryFactWritten events, got %d", len(written))
	}
	for i, ev := range written {
		payload := decodePayload(t, ev)
		if payload["source"] != "high_finding" {
			t.Fatalf("event[%d] source: want high_finding, got %v", i, payload["source"])
		}
		if payload["kind"] != "lint_rule" {
			t.Fatalf("event[%d] kind: want lint_rule, got %v", i, payload["kind"])
		}
		if _, ok := payload["id"]; !ok {
			t.Fatalf("event[%d] missing id field: %+v", i, payload)
		}
	}
}

func TestWriteHighFindingsToMemory_EmitsDBErrOnWriteFailure(t *testing.T) {
	m := newMemTestSetup(t)
	// Drop the table out from under the store to force every Write to fail.
	if _, err := m.deps.Store.DB.Exec(`DROP TABLE institutional_facts`); err != nil {
		t.Fatalf("drop table: %v", err)
	}
	m.sup.writeHighFindingsToMemory(m.sessionID, "dev", &FindingsReport{
		Findings: []Finding{
			{Critic: "c", ID: "1", Severity: SevHigh, Title: "A"},
			{Critic: "c", ID: "2", Severity: SevHigh, Title: "B"},
		},
	})

	events := m.drainEvents()
	if got := eventsOfKind(events, trajectory.MemoryFactWritten); len(got) != 0 {
		t.Fatalf("no MemoryFactWritten expected on failed writes, got %d", len(got))
	}
	// dbErr surfaces as a SubtaskStdout event carrying db_error + op.
	var sawDBErr int
	for _, ev := range events {
		if ev.Kind != trajectory.SubtaskStdout {
			continue
		}
		payload := decodePayload(t, ev)
		if payload["op"] == "memory_write_lint_rule" && payload["db_error"] != nil {
			sawDBErr++
		}
	}
	if sawDBErr != 2 {
		t.Fatalf("expected 2 memory_write_lint_rule db_error events, got %d", sawDBErr)
	}
}

func TestConsolidateAuditOutcome_NoMemoryDep(t *testing.T) {
	deps := newTestDeps(t)
	sup := New(deps)
	// Must not panic.
	sup.consolidateAuditOutcome("sess", ReflectorRequest{Goal: "g", ModeName: "audit"}, &ReflectorResult{Status: "completed"})
}

func TestConsolidateAuditOutcome_WritesSessionOutcome(t *testing.T) {
	m := newMemTestSetup(t)
	report := &FindingsReport{
		Findings: []Finding{
			{Critic: "c", ID: "1", Severity: SevHigh, Title: "h"},
			{Critic: "c", ID: "2", Severity: SevLow, Title: "l"},
		},
		Stats: map[string]int{"high": 1, "low": 1},
	}
	m.sup.consolidateAuditOutcome(m.sessionID, ReflectorRequest{
		Goal:     "review the auth middleware",
		ModeName: "audit",
	}, &ReflectorResult{
		Status:         "completed",
		Iterations:     2,
		StoppedBecause: string(StopAtNoHigh),
		FinalFindings:  report,
	})

	facts := allFacts(t, m.deps.Store)
	if len(facts) != 1 {
		t.Fatalf("expected 1 session_outcome fact, got %d", len(facts))
	}
	f := facts[0]
	if f.Kind != "session_outcome" {
		t.Fatalf("Kind: want session_outcome, got %q", f.Kind)
	}
	if f.SourceMode != "audit" {
		t.Fatalf("SourceMode: want audit, got %q", f.SourceMode)
	}
	for _, want := range []string{"review the auth middleware", "completed", "Iterations: 2", "no_high_findings", "Highest severity: high"} {
		if !strings.Contains(f.Body, want) {
			t.Fatalf("body missing %q: %q", want, f.Body)
		}
	}
	tagSet := map[string]bool{}
	for _, tag := range f.Tags {
		tagSet[tag] = true
	}
	// "session_outcome" normalizes to "session" + "outcome"; goal tokens
	// pass through NormalizeTags too.
	for _, want := range []string{"audit", "session", "outcome", "review", "auth", "middleware"} {
		if !tagSet[want] {
			t.Fatalf("tags missing %q: %v", want, f.Tags)
		}
	}

	written := eventsOfKind(m.drainEvents(), trajectory.MemoryFactWritten)
	if len(written) != 1 {
		t.Fatalf("expected 1 MemoryFactWritten event, got %d", len(written))
	}
	payload := decodePayload(t, written[0])
	if payload["source"] != "consolidate" {
		t.Fatalf("source: want consolidate, got %v", payload["source"])
	}
	if payload["kind"] != "session_outcome" {
		t.Fatalf("kind: want session_outcome, got %v", payload["kind"])
	}
}

func TestConsolidateAuditOutcome_NilFinalFindings(t *testing.T) {
	m := newMemTestSetup(t)
	// FinalFindings nil: highest defaults to SevUnknown, stats is nil.
	m.sup.consolidateAuditOutcome(m.sessionID, ReflectorRequest{
		Goal:     "investigate flaky test",
		ModeName: "audit",
	}, &ReflectorResult{Status: "failed", Iterations: 1, StoppedBecause: "max_iterations"})

	facts := allFacts(t, m.deps.Store)
	if len(facts) != 1 {
		t.Fatalf("expected 1 fact even without final findings, got %d", len(facts))
	}
	if !strings.Contains(facts[0].Body, "Highest severity: unknown") {
		t.Fatalf("body: %q", facts[0].Body)
	}
}

func TestConsolidateAuditOutcome_EmitsDBErrOnWriteFailure(t *testing.T) {
	m := newMemTestSetup(t)
	if _, err := m.deps.Store.DB.Exec(`DROP TABLE institutional_facts`); err != nil {
		t.Fatalf("drop table: %v", err)
	}
	m.sup.consolidateAuditOutcome(m.sessionID, ReflectorRequest{Goal: "g", ModeName: "audit"}, &ReflectorResult{Status: "completed"})

	events := m.drainEvents()
	if got := eventsOfKind(events, trajectory.MemoryFactWritten); len(got) != 0 {
		t.Fatalf("no MemoryFactWritten expected on failed write, got %d", len(got))
	}
	var sawDBErr bool
	for _, ev := range events {
		if ev.Kind != trajectory.SubtaskStdout {
			continue
		}
		p := decodePayload(t, ev)
		if p["op"] == "memory_write_session_outcome" && p["db_error"] != nil {
			sawDBErr = true
		}
	}
	if !sawDBErr {
		t.Fatal("expected memory_write_session_outcome db_error event")
	}
}

func TestConsolidateRunOutcome_NoMemoryDep(t *testing.T) {
	deps := newTestDeps(t)
	sup := New(deps)
	sup.consolidateRunOutcome("sess", RunRequest{Goal: "g", ModeName: "dev"}, &RunResult{Status: "completed"})
}

func TestConsolidateRunOutcome_WritesSessionOutcome(t *testing.T) {
	m := newMemTestSetup(t)
	m.sup.consolidateRunOutcome(m.sessionID, RunRequest{
		Goal:     "ship the cli refactor",
		ModeName: "dev",
	}, &RunResult{
		Status: "completed",
		Subtasks: []store.Subtask{
			{ID: "a"}, {ID: "b"}, {ID: "c"},
		},
	})

	facts := allFacts(t, m.deps.Store)
	if len(facts) != 1 {
		t.Fatalf("expected 1 fact, got %d", len(facts))
	}
	f := facts[0]
	if f.Kind != "session_outcome" {
		t.Fatalf("Kind: %q", f.Kind)
	}
	if f.SourceMode != "dev" {
		t.Fatalf("SourceMode: %q", f.SourceMode)
	}
	for _, want := range []string{"ship the cli refactor", "Status: completed", "Subtasks: 3"} {
		if !strings.Contains(f.Body, want) {
			t.Fatalf("body missing %q: %q", want, f.Body)
		}
	}

	written := eventsOfKind(m.drainEvents(), trajectory.MemoryFactWritten)
	if len(written) != 1 {
		t.Fatalf("expected 1 MemoryFactWritten, got %d", len(written))
	}
	if decodePayload(t, written[0])["source"] != "consolidate" {
		t.Fatal("MemoryFactWritten payload source should be consolidate")
	}
}

func TestConsolidateRunOutcome_EmitsDBErrOnWriteFailure(t *testing.T) {
	m := newMemTestSetup(t)
	if _, err := m.deps.Store.DB.Exec(`DROP TABLE institutional_facts`); err != nil {
		t.Fatalf("drop table: %v", err)
	}
	m.sup.consolidateRunOutcome(m.sessionID, RunRequest{Goal: "g", ModeName: "dev"}, &RunResult{Status: "completed"})

	events := m.drainEvents()
	if got := eventsOfKind(events, trajectory.MemoryFactWritten); len(got) != 0 {
		t.Fatalf("no MemoryFactWritten expected on failed write, got %d", len(got))
	}
	var sawDBErr bool
	for _, ev := range events {
		if ev.Kind != trajectory.SubtaskStdout {
			continue
		}
		p := decodePayload(t, ev)
		if p["op"] == "memory_write_session_outcome" && p["db_error"] != nil {
			sawDBErr = true
		}
	}
	if !sawDBErr {
		t.Fatal("expected memory_write_session_outcome db_error event")
	}
}

func TestRelevantMemoryBlock_NoMemoryDep(t *testing.T) {
	deps := newTestDeps(t)
	sup := New(deps)
	if got := sup.relevantMemoryBlock("sess", "goal text", 3); got != "" {
		t.Fatalf("expected empty block with nil Memory, got %q", got)
	}
}

func TestRelevantMemoryBlock_NegativeTopKDisables(t *testing.T) {
	m := newMemTestSetup(t)
	// Seed a fact that would otherwise match.
	if _, err := m.deps.Memory.Write(memory.Fact{
		Kind: "lint_rule",
		Body: "be careful with reentrancy",
		Tags: []string{"reentrancy"},
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if got := m.sup.relevantMemoryBlock(m.sessionID, "fix reentrancy bug", -1); got != "" {
		t.Fatalf("expected empty block with topK<0, got %q", got)
	}
}

func TestRelevantMemoryBlock_EmptyGoalTokensReturnsEmpty(t *testing.T) {
	m := newMemTestSetup(t)
	if _, err := m.deps.Memory.Write(memory.Fact{
		Kind: "lint_rule",
		Body: "rule",
		Tags: []string{"x"},
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	// Only stop-words / punctuation tokenize to nothing.
	if got := m.sup.relevantMemoryBlock(m.sessionID, "the and for", 3); got != "" {
		t.Fatalf("expected empty block when goal tokenizes to nothing, got %q", got)
	}
	if got := eventsOfKind(m.drainEvents(), trajectory.MemoryFactsInjected); len(got) != 0 {
		t.Fatalf("no MemoryFactsInjected expected, got %d", len(got))
	}
}

func TestRelevantMemoryBlock_NoMatchesReturnsEmpty(t *testing.T) {
	m := newMemTestSetup(t)
	if _, err := m.deps.Memory.Write(memory.Fact{
		Kind: "lint_rule",
		Body: "rule about solidity",
		Tags: []string{"solidity", "reentrancy"},
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if got := m.sup.relevantMemoryBlock(m.sessionID, "refactor the python http client", 3); got != "" {
		t.Fatalf("expected empty block when no overlap, got %q", got)
	}
}

func TestRelevantMemoryBlock_RendersAndEmits(t *testing.T) {
	m := newMemTestSetup(t)
	if _, err := m.deps.Memory.Write(memory.Fact{
		SourceMode: "audit",
		Kind:       "lint_rule",
		Body:       "Reentrancy in withdraw\n\nCheck the state update.",
		Tags:       []string{"reentrancy", "solidity"},
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	got := m.sup.relevantMemoryBlock(m.sessionID, "fix reentrancy in vault", 3)
	if got == "" {
		t.Fatal("expected non-empty block")
	}
	if !strings.HasPrefix(got, "## Relevant prior facts\n\n") {
		t.Fatalf("block missing header: %q", got)
	}
	if !strings.HasSuffix(got, "\n\n") {
		t.Fatalf("block should end with trailing blank line: %q", got)
	}
	if !strings.Contains(got, "[audit · lint_rule]") {
		t.Fatalf("block missing source/kind prefix: %q", got)
	}
	// Multi-line bodies collapse to the first line in the block.
	if !strings.Contains(got, "Reentrancy in withdraw") {
		t.Fatalf("block missing first body line: %q", got)
	}
	if strings.Contains(got, "Check the state update.") {
		t.Fatalf("block should not contain post-newline body text: %q", got)
	}

	injected := eventsOfKind(m.drainEvents(), trajectory.MemoryFactsInjected)
	if len(injected) != 1 {
		t.Fatalf("expected 1 MemoryFactsInjected event, got %d", len(injected))
	}
	payload := decodePayload(t, injected[0])
	if cnt, ok := payload["count"].(float64); !ok || int(cnt) != 1 {
		t.Fatalf("count: want 1, got %v", payload["count"])
	}
	if tk, ok := payload["top_k"].(float64); !ok || int(tk) != 3 {
		t.Fatalf("top_k: want 3, got %v", payload["top_k"])
	}
}

func TestRelevantMemoryBlock_ZeroTopKUsesDefault(t *testing.T) {
	m := newMemTestSetup(t)
	if _, err := m.deps.Memory.Write(memory.Fact{
		Kind: "lint_rule",
		Body: "rule about reentrancy",
		Tags: []string{"reentrancy"},
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	_ = m.sup.relevantMemoryBlock(m.sessionID, "fix reentrancy", 0)
	injected := eventsOfKind(m.drainEvents(), trajectory.MemoryFactsInjected)
	if len(injected) != 1 {
		t.Fatalf("expected 1 MemoryFactsInjected event, got %d", len(injected))
	}
	payload := decodePayload(t, injected[0])
	if tk, ok := payload["top_k"].(float64); !ok || int(tk) != defaultMemoryTopK {
		t.Fatalf("top_k: want default %d, got %v", defaultMemoryTopK, payload["top_k"])
	}
}

// TestRunPlanner_PrependsMemoryBlock locks down the contract that the
// "## Relevant prior facts" block is actually prepended onto the planner
// prompt before the provider is invoked — not just that
// relevantMemoryBlock returns a non-empty string in isolation. Drives
// runPlanner with a fake planner that captures the prompt.
func TestRunPlanner_PrependsMemoryBlock(t *testing.T) {
	m := newMemTestSetup(t)
	if _, err := m.deps.Memory.Write(memory.Fact{
		SourceMode: "audit",
		Kind:       "lint_rule",
		Body:       "Reentrancy: check state update before external call.",
		Tags:       []string{"reentrancy", "solidity"},
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	var captured string
	planner := &fakeProvider{
		name: "planner",
		run: func(ctx context.Context, prompt string, opts provider.RunOptions, events chan<- provider.Event) (provider.RunResult, error) {
			captured = prompt
			return provider.RunResult{FinalText: `{"subtasks":[{"id":"s1","title":"t","prompt":"p"}]}`}, nil
		},
	}
	if err := m.deps.Registry.Register(planner, false); err != nil {
		t.Fatalf("register planner: %v", err)
	}

	if _, err := m.sup.runPlanner(context.Background(), m.sessionID, planner, RunRequest{
		Goal:           "fix reentrancy in vault",
		WorkerName:     "planner",
		SubtaskTimeout: 5 * time.Second,
		MemoryTopK:     3,
	}); err != nil {
		t.Fatalf("runPlanner: %v", err)
	}

	if !strings.Contains(captured, "## Relevant prior facts") {
		t.Fatalf("planner prompt missing memory header; got: %q", captured)
	}
	if !strings.Contains(captured, "Reentrancy: check state update") {
		t.Fatalf("planner prompt missing seeded fact body; got: %q", captured)
	}
	if !strings.Contains(captured, "fix reentrancy in vault") {
		t.Fatalf("planner prompt missing goal text; got: %q", captured)
	}
	memIdx := strings.Index(captured, "## Relevant prior facts")
	goalIdx := strings.Index(captured, "fix reentrancy in vault")
	if memIdx < 0 || goalIdx < 0 || memIdx >= goalIdx {
		t.Fatalf("memory block should appear *before* the goal; memIdx=%d goalIdx=%d prompt=%q", memIdx, goalIdx, captured)
	}
}

// TestRunPlanner_NoMemoryBlockWhenStoreEmpty is the negative companion:
// with no facts seeded the planner prompt is just the rendered template
// — no header is injected and the goal is still present.
func TestRunPlanner_NoMemoryBlockWhenStoreEmpty(t *testing.T) {
	m := newMemTestSetup(t)

	var captured string
	planner := &fakeProvider{
		name: "planner",
		run: func(ctx context.Context, prompt string, opts provider.RunOptions, events chan<- provider.Event) (provider.RunResult, error) {
			captured = prompt
			return provider.RunResult{FinalText: `{"subtasks":[{"id":"s1","title":"t","prompt":"p"}]}`}, nil
		},
	}
	if err := m.deps.Registry.Register(planner, false); err != nil {
		t.Fatalf("register planner: %v", err)
	}

	if _, err := m.sup.runPlanner(context.Background(), m.sessionID, planner, RunRequest{
		Goal:           "ship the cli refactor",
		WorkerName:     "planner",
		SubtaskTimeout: 5 * time.Second,
		MemoryTopK:     3,
	}); err != nil {
		t.Fatalf("runPlanner: %v", err)
	}

	if strings.Contains(captured, "## Relevant prior facts") {
		t.Fatalf("planner prompt should not contain memory header when store is empty; got: %q", captured)
	}
	if !strings.Contains(captured, "ship the cli refactor") {
		t.Fatalf("planner prompt missing goal text; got: %q", captured)
	}
}

func TestRelevantMemoryBlock_NoSourceModeRendersAsNoMode(t *testing.T) {
	m := newMemTestSetup(t)
	if _, err := m.deps.Memory.Write(memory.Fact{
		SourceMode: "",
		Kind:       "lint_rule",
		Body:       "anonymous rule",
		Tags:       []string{"goalword"},
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	got := m.sup.relevantMemoryBlock(m.sessionID, "goalword something", 3)
	if !strings.Contains(got, "[no-mode · lint_rule]") {
		t.Fatalf("expected no-mode placeholder for empty source: %q", got)
	}
}

func TestFindingTags_FileAndExtension(t *testing.T) {
	f := &Finding{
		Critic:   "sol-critic",
		Severity: SevHigh,
		Title:    "Reentrancy in withdraw",
		File:     "contracts/Vault.sol",
	}
	// findingTags returns the raw composed slice; persistence-time
	// normalization (lowercasing, splitting on punctuation, deduping) is
	// memory.Store.Write's job. Both shapes are part of the contract — we
	// assert the raw composition here and the normalized form in the
	// write-side test above.
	got := findingTags(f, "audit")
	want := []string{"high", "finding", "sol-critic", "audit", "sol", "Vault.sol", "Reentrancy in withdraw"}
	gotSet := map[string]bool{}
	for _, g := range got {
		gotSet[g] = true
	}
	for _, w := range want {
		if !gotSet[w] {
			t.Fatalf("findingTags missing raw element %q: %v", w, got)
		}
	}
}

func TestFindingTags_NoFileSkipsExtension(t *testing.T) {
	f := &Finding{Critic: "c", Severity: SevHigh, Title: "t"}
	got := findingTags(f, "m")
	for _, tag := range got {
		if strings.HasPrefix(tag, ".") {
			t.Fatalf("unexpected leading-dot ext token: %v", got)
		}
	}
}

func TestConsolidateTagsFromGoal(t *testing.T) {
	// Same raw-vs-normalized split as findingTags: this helper composes,
	// memory.Store.Write normalizes at persistence time.
	got := consolidateTagsFromGoal("review the auth middleware", "audit")
	gotSet := map[string]bool{}
	for _, g := range got {
		gotSet[g] = true
	}
	for _, w := range []string{"audit", "session_outcome", "review the auth middleware"} {
		if !gotSet[w] {
			t.Fatalf("missing raw element %q: %v", w, got)
		}
	}
}

// TestRunReflector_ConsolidateGateRespected drives RunReflector end-to-end
// and locks down the gate at reflector.go: a session_outcome fact is
// written only when ReflectorRequest.MemoryConsolidate is true. This is
// the integration counterpart to TestConsolidateAuditOutcome_* — those
// hit the hook in isolation, this one proves the hook actually fires
// from the audit flow.
func TestRunReflector_ConsolidateGateRespected(t *testing.T) {
	for _, tc := range []struct {
		name        string
		consolidate bool
		wantFacts   int
	}{
		{"flag_off_no_write", false, 0},
		{"flag_on_writes_outcome", true, 1},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			deps := newTestDeps(t)
			deps.Memory = memory.New(deps.Store.DB)
			worker := &fakeProvider{
				name: "worker",
				run: func(ctx context.Context, prompt string, opts provider.RunOptions, events chan<- provider.Event) (provider.RunResult, error) {
					return provider.RunResult{FinalText: `{"findings":[]}`}, nil
				},
			}
			if err := deps.Registry.Register(worker, false); err != nil {
				t.Fatalf("register: %v", err)
			}

			if _, err := New(deps).RunReflector(context.Background(), ReflectorRequest{
				Goal:              "audit something",
				InputSummary:      "the code under ./contracts",
				DefaultWorker:     "worker",
				Critics:           []CriticSpec{{ID: "c1", Prompt: "lens"}},
				StopWhen:          StopAtNoFindings,
				MaxIterations:     1,
				ModeName:          "audit",
				MemoryConsolidate: tc.consolidate,
			}); err != nil {
				t.Fatalf("RunReflector: %v", err)
			}

			facts := allFacts(t, deps.Store)
			if len(facts) != tc.wantFacts {
				t.Fatalf("facts: got %d want %d (%+v)", len(facts), tc.wantFacts, facts)
			}
			if tc.consolidate {
				if facts[0].Kind != "session_outcome" {
					t.Fatalf("Kind: got %q want session_outcome", facts[0].Kind)
				}
				if facts[0].SourceMode != "audit" {
					t.Fatalf("SourceMode: got %q want audit", facts[0].SourceMode)
				}
			}
		})
	}
}

func TestOneLineBody(t *testing.T) {
	if got := oneLineBody("  first\nsecond\nthird  "); got != "first" {
		t.Fatalf("multi-line: got %q", got)
	}
	if got := oneLineBody("only one"); got != "only one" {
		t.Fatalf("single-line: got %q", got)
	}
	if got := oneLineBody(""); got != "" {
		t.Fatalf("empty: got %q", got)
	}
	long := strings.Repeat("a", 300)
	got := oneLineBody(long)
	if !strings.HasSuffix(got, "…") {
		t.Fatalf("expected ellipsis truncation, got %q", got)
	}
	// 240 bytes of "a" + "…"
	if !strings.HasPrefix(got, strings.Repeat("a", 240)) {
		t.Fatalf("truncated prefix mismatch: %q", got)
	}
}
