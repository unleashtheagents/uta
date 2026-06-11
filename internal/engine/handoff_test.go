package engine

import (
	"testing"
	"time"

	"github.com/unleashtheagents/uta/internal/profile"
	"github.com/unleashtheagents/uta/internal/store"
)

func makeSessionStub(id string) store.Session {
	return store.Session{
		ID:        id,
		Goal:      "test goal",
		Worker:    "worker",
		Status:    "completed",
		CreatedAt: time.Now(),
	}
}

func TestApplyProfile_PropagatesOnComplete(t *testing.T) {
	p := &profile.MissionProfile{
		Name: "dev",
		OnComplete: []profile.Handoff{
			{TargetMode: "audit", PromptTemplate: "audit it"},
		},
	}
	req := &RunRequest{}
	ApplyProfile(req, p)
	if len(req.OnComplete) != 1 || req.OnComplete[0].TargetMode != "audit" {
		t.Errorf("OnComplete not propagated: %+v", req.OnComplete)
	}
	// Verify defensive copy: mutating the request's slice must not leak
	// back into the source profile.
	req.OnComplete[0].TargetMode = "mutated"
	if p.OnComplete[0].TargetMode == "mutated" {
		t.Error("ApplyProfile must defensively copy OnComplete")
	}
}

func TestApplyProfile_PropagatesMaxHandoffDepth(t *testing.T) {
	p := &profile.MissionProfile{Name: "dev", MaxHandoffDepth: 7}
	req := &RunRequest{}
	ApplyProfile(req, p)
	if req.MaxHandoffDepth != 7 {
		t.Errorf("MaxHandoffDepth = %d, want 7", req.MaxHandoffDepth)
	}
	// Zero value passes through as zero (caller treats it as "use default").
	p.MaxHandoffDepth = 0
	req = &RunRequest{}
	ApplyProfile(req, p)
	if req.MaxHandoffDepth != 0 {
		t.Errorf("zero MaxHandoffDepth should pass through as 0, got %d", req.MaxHandoffDepth)
	}
}

func TestEvaluateHandoffs_FirstMatchWins(t *testing.T) {
	deps := newTestDeps(t)
	sup := New(deps)

	auditProfile := &profile.MissionProfile{Name: "audit"}
	opsProfile := &profile.MissionProfile{Name: "ops"}
	resolve := func(name string) (*profile.MissionProfile, error) {
		switch name {
		case "audit":
			return auditProfile, nil
		case "ops":
			return opsProfile, nil
		}
		return nil, nil
	}

	handoffs := []profile.Handoff{
		{TargetMode: "audit", Condition: profile.HandoffCondition{ContainsAny: []string{"*.sol"}}, PromptTemplate: "audit {{prior_session_id}}"},
		{TargetMode: "ops"},
	}

	// First condition matches — audit wins.
	if err := deps.Store.CreateSession(makeSessionStub("prior-1")); err != nil {
		t.Fatalf("create session: %v", err)
	}
	m, err := sup.EvaluateHandoffs("prior-1", handoffs, []string{"Token.sol"}, resolve)
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if m == nil {
		t.Fatal("expected a match")
	}
	if m.Handoff.TargetMode != "audit" {
		t.Errorf("matched %q, want audit", m.Handoff.TargetMode)
	}
	if m.Prompt != "audit prior-1" {
		t.Errorf("prompt = %q", m.Prompt)
	}

	// First condition fails — ops (unconditional) wins.
	if err := deps.Store.CreateSession(makeSessionStub("prior-2")); err != nil {
		t.Fatalf("create session: %v", err)
	}
	m, err = sup.EvaluateHandoffs("prior-2", handoffs, []string{"README.md"}, resolve)
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if m == nil || m.Handoff.TargetMode != "ops" {
		t.Fatalf("expected ops match, got %+v", m)
	}
}

func TestEvaluateHandoffs_NoTargetSkipsAndContinues(t *testing.T) {
	deps := newTestDeps(t)
	sup := New(deps)

	resolve := func(name string) (*profile.MissionProfile, error) {
		if name == "audit" {
			return &profile.MissionProfile{Name: "audit"}, nil
		}
		return nil, nil
	}

	handoffs := []profile.Handoff{
		{TargetMode: "missing"},
		{TargetMode: "audit"},
	}
	if err := deps.Store.CreateSession(makeSessionStub("prior-3")); err != nil {
		t.Fatalf("create session: %v", err)
	}
	m, err := sup.EvaluateHandoffs("prior-3", handoffs, nil, resolve)
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if m == nil || m.Handoff.TargetMode != "audit" {
		t.Fatalf("expected audit match after skipping missing, got %+v", m)
	}
}

func TestEvaluateHandoffs_NoMatchReturnsNil(t *testing.T) {
	deps := newTestDeps(t)
	sup := New(deps)

	resolve := func(name string) (*profile.MissionProfile, error) {
		return nil, nil
	}
	if err := deps.Store.CreateSession(makeSessionStub("prior-4")); err != nil {
		t.Fatalf("create session: %v", err)
	}
	m, err := sup.EvaluateHandoffs("prior-4", []profile.Handoff{{TargetMode: "x"}}, nil, resolve)
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if m != nil {
		t.Errorf("expected nil match, got %+v", m)
	}
}
