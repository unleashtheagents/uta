package engine

import (
	"strings"
	"testing"
)

func TestValidateDAG_Empty(t *testing.T) {
	if err := validateDAG(Plan{}); err != nil {
		t.Fatalf("empty plan should be valid, got: %v", err)
	}
}

func TestValidateDAG_SingleSubtaskNoDeps(t *testing.T) {
	plan := Plan{Subtasks: []SubtaskSpec{{ID: "s1"}}}
	if err := validateDAG(plan); err != nil {
		t.Fatalf("single subtask should be valid, got: %v", err)
	}
}

func TestValidateDAG_LinearChain(t *testing.T) {
	plan := Plan{Subtasks: []SubtaskSpec{
		{ID: "s1"},
		{ID: "s2", Needs: []string{"s1"}},
		{ID: "s3", Needs: []string{"s2"}},
	}}
	if err := validateDAG(plan); err != nil {
		t.Fatalf("linear chain should be valid, got: %v", err)
	}
}

func TestValidateDAG_Diamond(t *testing.T) {
	plan := Plan{Subtasks: []SubtaskSpec{
		{ID: "s1"},
		{ID: "s2", Needs: []string{"s1"}},
		{ID: "s3", Needs: []string{"s1"}},
		{ID: "s4", Needs: []string{"s2", "s3"}},
	}}
	if err := validateDAG(plan); err != nil {
		t.Fatalf("diamond should be valid, got: %v", err)
	}
}

func TestValidateDAG_DisconnectedComponents(t *testing.T) {
	plan := Plan{Subtasks: []SubtaskSpec{
		{ID: "a1"},
		{ID: "a2", Needs: []string{"a1"}},
		{ID: "b1"},
		{ID: "b2", Needs: []string{"b1"}},
	}}
	if err := validateDAG(plan); err != nil {
		t.Fatalf("disconnected components should be valid, got: %v", err)
	}
}

func TestValidateDAG_EmptyID(t *testing.T) {
	plan := Plan{Subtasks: []SubtaskSpec{
		{ID: "s1"},
		{ID: ""},
	}}
	err := validateDAG(plan)
	if err == nil {
		t.Fatal("expected error for empty id, got nil")
	}
	if !strings.Contains(err.Error(), "empty id") {
		t.Fatalf("expected 'empty id' error, got: %v", err)
	}
}

func TestValidateDAG_DuplicateID(t *testing.T) {
	plan := Plan{Subtasks: []SubtaskSpec{
		{ID: "s1"},
		{ID: "s1"},
	}}
	err := validateDAG(plan)
	if err == nil {
		t.Fatal("expected error for duplicate id, got nil")
	}
	if !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("expected 'duplicate' error, got: %v", err)
	}
}

func TestValidateDAG_MissingDependency(t *testing.T) {
	plan := Plan{Subtasks: []SubtaskSpec{
		{ID: "s1", Needs: []string{"ghost"}},
	}}
	err := validateDAG(plan)
	if err == nil {
		t.Fatal("expected error for missing dependency, got nil")
	}
	if !strings.Contains(err.Error(), "does not exist") {
		t.Fatalf("expected 'does not exist' error, got: %v", err)
	}
}

func TestValidateDAG_SelfCycle(t *testing.T) {
	plan := Plan{Subtasks: []SubtaskSpec{
		{ID: "s1", Needs: []string{"s1"}},
	}}
	err := validateDAG(plan)
	if err == nil {
		t.Fatal("expected error for self cycle, got nil")
	}
	if !strings.Contains(err.Error(), "cycle") {
		t.Fatalf("expected 'cycle' error, got: %v", err)
	}
}

func TestValidateDAG_TwoNodeCycle(t *testing.T) {
	plan := Plan{Subtasks: []SubtaskSpec{
		{ID: "s1", Needs: []string{"s2"}},
		{ID: "s2", Needs: []string{"s1"}},
	}}
	err := validateDAG(plan)
	if err == nil {
		t.Fatal("expected error for two-node cycle, got nil")
	}
	if !strings.Contains(err.Error(), "cycle") {
		t.Fatalf("expected 'cycle' error, got: %v", err)
	}
}

func TestValidateDAG_LongCycle(t *testing.T) {
	plan := Plan{Subtasks: []SubtaskSpec{
		{ID: "s1", Needs: []string{"s3"}},
		{ID: "s2", Needs: []string{"s1"}},
		{ID: "s3", Needs: []string{"s2"}},
	}}
	err := validateDAG(plan)
	if err == nil {
		t.Fatal("expected error for three-node cycle, got nil")
	}
	if !strings.Contains(err.Error(), "cycle") {
		t.Fatalf("expected 'cycle' error, got: %v", err)
	}
}

func TestValidateDAG_CycleWithIndependentValidNodes(t *testing.T) {
	plan := Plan{Subtasks: []SubtaskSpec{
		{ID: "ok1"},
		{ID: "ok2", Needs: []string{"ok1"}},
		{ID: "bad1", Needs: []string{"bad2"}},
		{ID: "bad2", Needs: []string{"bad1"}},
	}}
	err := validateDAG(plan)
	if err == nil {
		t.Fatal("expected error when a cycle exists alongside valid nodes, got nil")
	}
	if !strings.Contains(err.Error(), "cycle") {
		t.Fatalf("expected 'cycle' error, got: %v", err)
	}
}
