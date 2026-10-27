package engine

import (
	"context"
	"strings"
	"testing"

	"github.com/unleashtheagents/uta/internal/provider"
)

type roleEchoProvider struct{ roles []provider.Role }

func (p *roleEchoProvider) Name() string { return "echo" }
func (p *roleEchoProvider) Detect(context.Context) provider.Detection {
	return provider.Detection{Available: true}
}
func (p *roleEchoProvider) RunHeadless(_ context.Context, prompt string, opts provider.RunOptions, _ chan<- provider.Event) (provider.RunResult, error) {
	p.roles = append(p.roles, opts.Role)
	if opts.Role == provider.RoleCompact {
		return provider.RunResult{FinalText: "DIGEST of " + prompt[strings.Index(prompt, "Subtask: ")+9:][:5], TokensIn: 10, TokensOut: 2}, nil
	}
	return provider.RunResult{FinalText: "x"}, nil
}

func TestCompactOutcomesOnlyOversized(t *testing.T) {
	deps := newTestDeps(t)
	s := New(deps)
	prov := &roleEchoProvider{}
	long := strings.Repeat("long worker transcript ", 100) // ~2300 chars
	outcomes := []SubtaskOutcome{
		{ID: "a", Title: "alpha", Result: long},
		{ID: "b", Title: "bravo", Result: "short"},
		{ID: "c", Title: "charlie", Result: long, Failed: true},
	}
	got := s.compactOutcomes(context.Background(), "sess", prov, outcomes, RunRequest{CompactOutputsOver: 1000})
	if !strings.HasPrefix(got[0].Result, "DIGEST of alpha") {
		t.Errorf("oversized result not compacted: %q", got[0].Result[:40])
	}
	if got[1].Result != "short" || got[2].Result != long {
		t.Errorf("short and failed outcomes must be untouched")
	}
	if len(prov.roles) != 1 || prov.roles[0] != provider.RoleCompact {
		t.Errorf("expected exactly one compact-role call, got %v", prov.roles)
	}
	if outcomes[0].Result != long {
		t.Errorf("input slice must not be mutated")
	}
	// Disabled: nothing happens.
	prov.roles = nil
	if got := s.compactOutcomes(context.Background(), "sess", prov, outcomes, RunRequest{}); got[0].Result != long || len(prov.roles) != 0 {
		t.Errorf("threshold 0 must be a no-op")
	}
}
