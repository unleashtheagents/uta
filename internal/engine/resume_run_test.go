package engine

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/unleashtheagents/uta/internal/provider"
	"github.com/unleashtheagents/uta/internal/store"
)

// seedCrashedRun creates a session that died mid-flight: two completed
// subtasks with results, one that was still running at crash, one never
// started. Prompt blobs are written for all four so re-run recovery has
// real prompts to read. Returns the session id.
func seedCrashedRun(t *testing.T, deps Deps) string {
	t.Helper()
	const sessionID = "crashed-1"
	if err := deps.Store.CreateSession(store.Session{
		ID: sessionID, Goal: "build the report", Worker: "worker",
		Planner: "worker", Status: "failed", CreatedAt: time.Now().Add(-time.Hour),
		ModeName: "dev",
	}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	type row struct {
		id, spec, title, prompt, status, result string
	}
	rows := []row{
		{"st-a", "s1", "gather data", "prompt: gather the data", "completed", "the gathered data"},
		{"st-b", "s2", "analyze", "prompt: analyze it", "completed", "the analysis"},
		{"st-c", "s3", "draft report", "prompt: draft the report", "running", ""},
		{"st-d", "s4", "review", "prompt: review the draft", "pending", ""},
	}
	start := time.Now().Add(-30 * time.Minute)
	done := time.Now().Add(-20 * time.Minute)
	for i, r := range rows {
		promptRef, err := deps.Blobs.Put([]byte(r.prompt), "txt")
		if err != nil {
			t.Fatalf("blob put: %v", err)
		}
		if err := deps.Store.CreateSubtask(store.Subtask{
			ID: r.id, SessionID: sessionID, Ord: i, SpecID: r.spec,
			Title: r.title, PromptRef: promptRef, Worker: "worker", Status: "pending",
		}); err != nil {
			t.Fatalf("CreateSubtask(%s): %v", r.id, err)
		}
		switch r.status {
		case "completed":
			if err := deps.Store.UpdateSubtask(store.Subtask{
				ID: r.id, Status: "completed",
				StartedAt: &start, CompletedAt: &done, ResultText: r.result,
			}); err != nil {
				t.Fatalf("UpdateSubtask(%s): %v", r.id, err)
			}
		case "running":
			if err := deps.Store.UpdateSubtask(store.Subtask{
				ID: r.id, Status: "running", StartedAt: &start,
			}); err != nil {
				t.Fatalf("UpdateSubtask(%s): %v", r.id, err)
			}
		}
	}
	return sessionID
}

func TestBuildResumeRunRequest_SplitsCompletedAndRemainder(t *testing.T) {
	deps := newTestDeps(t)
	sessionID := seedCrashedRun(t, deps)

	req, info, err := BuildResumeRunRequest(deps.Store, deps.Blobs, sessionID)
	if err != nil {
		t.Fatalf("BuildResumeRunRequest: %v", err)
	}
	if info.Completed != 2 || info.Rerun != 2 {
		t.Errorf("info = %+v, want Completed=2 Rerun=2", info)
	}
	if req.Goal != "build the report" || req.WorkerName != "worker" || req.ModeName != "dev" {
		t.Errorf("recovered identity wrong: %+v", req)
	}
	if req.ResumedFromSession != sessionID {
		t.Errorf("ResumedFromSession = %q", req.ResumedFromSession)
	}
	if len(req.PriorOutcomes) != 2 || req.PriorOutcomes[0].Result != "the gathered data" {
		t.Errorf("prior outcomes wrong: %+v", req.PriorOutcomes)
	}
	if len(req.PreSetSubtasks) != 2 {
		t.Fatalf("preset subtasks = %d, want 2", len(req.PreSetSubtasks))
	}
	// Prompts must come back from the blobs verbatim.
	if req.PreSetSubtasks[0].Prompt != "prompt: draft the report" {
		t.Errorf("recovered prompt = %q", req.PreSetSubtasks[0].Prompt)
	}
}

func TestBuildResumeRunRequest_RejectsCompletedSession(t *testing.T) {
	deps := newTestDeps(t)
	if err := deps.Store.CreateSession(store.Session{
		ID: "done-1", Goal: "g", Worker: "w", Status: "completed", CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	_, _, err := BuildResumeRunRequest(deps.Store, deps.Blobs, "done-1")
	if err == nil || !strings.Contains(err.Error(), "already completed") {
		t.Fatalf("expected already-completed error, got %v", err)
	}
}

func TestBuildResumeRunRequest_UnknownSession(t *testing.T) {
	deps := newTestDeps(t)
	_, _, err := BuildResumeRunRequest(deps.Store, deps.Blobs, "ghost")
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("expected not-found error, got %v", err)
	}
}

func TestBuildResumeRunRequest_NothingToRerun(t *testing.T) {
	deps := newTestDeps(t)
	// Failed session whose only subtask completed: synthesis was the
	// failure. Must point the operator at `uta resume`, not re-run.
	if err := deps.Store.CreateSession(store.Session{
		ID: "synth-fail", Goal: "g", Worker: "w", Status: "failed", CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	ref, _ := deps.Blobs.Put([]byte("p"), "txt")
	if err := deps.Store.CreateSubtask(store.Subtask{
		ID: "st-1", SessionID: "synth-fail", Ord: 0, SpecID: "s1",
		Title: "t", PromptRef: ref, Worker: "w", Status: "pending",
	}); err != nil {
		t.Fatalf("CreateSubtask: %v", err)
	}
	start, done := time.Now(), time.Now()
	if err := deps.Store.UpdateSubtask(store.Subtask{
		ID: "st-1", Status: "completed", StartedAt: &start, CompletedAt: &done, ResultText: "r",
	}); err != nil {
		t.Fatalf("UpdateSubtask: %v", err)
	}
	_, _, err := BuildResumeRunRequest(deps.Store, deps.Blobs, "synth-fail")
	if err == nil || !strings.Contains(err.Error(), "no unfinished subtasks") {
		t.Fatalf("expected nothing-to-rerun error, got %v", err)
	}
}

// TestResumeRun_EndToEnd drives the full loop: seed a crashed session,
// build the resume request, run it against a fake provider, and assert
// (a) only the 2 unfinished subtasks were executed, (b) the synthesis
// prompt contained ALL FOUR results — recovered and fresh.
func TestResumeRun_EndToEnd(t *testing.T) {
	deps := newTestDeps(t)
	sessionID := seedCrashedRun(t, deps)

	var executedPrompts []string
	var synthPrompt string
	worker := &fakeProvider{
		name: "worker",
		run: func(_ context.Context, prompt string, _ provider.RunOptions, _ chan<- provider.Event) (provider.RunResult, error) {
			if strings.Contains(prompt, "synthesis step") {
				synthPrompt = prompt
				return provider.RunResult{FinalText: "the final report"}, nil
			}
			executedPrompts = append(executedPrompts, prompt)
			return provider.RunResult{FinalText: "fresh result for: " + prompt}, nil
		},
	}
	if err := deps.Registry.Register(worker, false); err != nil {
		t.Fatalf("register: %v", err)
	}

	req, _, err := BuildResumeRunRequest(deps.Store, deps.Blobs, sessionID)
	if err != nil {
		t.Fatalf("BuildResumeRunRequest: %v", err)
	}
	req.SubtaskTimeout = 30 * time.Second
	req.RunTimeout = time.Minute

	res, err := New(deps).Run(context.Background(), req)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Status != "completed" {
		t.Errorf("status = %q", res.Status)
	}
	if len(executedPrompts) != 2 {
		t.Errorf("executed %d subtask prompts, want 2 (completed ones must not re-run): %v",
			len(executedPrompts), executedPrompts)
	}
	for _, leak := range []string{"prompt: gather the data", "prompt: analyze it"} {
		for _, p := range executedPrompts {
			if p == leak {
				t.Errorf("completed subtask re-executed: %q", leak)
			}
		}
	}
	// Synthesis must see recovered + fresh results.
	for _, want := range []string{"the gathered data", "the analysis", "fresh result for"} {
		if !strings.Contains(synthPrompt, want) {
			t.Errorf("synthesis prompt missing %q", want)
		}
	}
	// Provenance: the new session's meta links back.
	sess, err := deps.Store.GetSession(res.SessionID)
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if !strings.Contains(sess.MetaJSON, sessionID) {
		t.Errorf("meta_json missing resumed_from link: %q", sess.MetaJSON)
	}
}
