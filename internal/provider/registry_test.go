package provider

import (
	"context"
	"testing"
)

type stubProvider struct {
	name      string
	available bool
}

func (s *stubProvider) Name() string { return s.name }
func (s *stubProvider) Detect(ctx context.Context) Detection {
	return Detection{Available: s.available, BinaryPath: "/fake/" + s.name}
}
func (s *stubProvider) RunHeadless(
	ctx context.Context,
	prompt string,
	opts RunOptions,
	events chan<- Event,
) (RunResult, error) {
	return RunResult{}, nil
}

func TestRegistry_GetAvailable(t *testing.T) {
	r := NewRegistry()
	mustRegister(t, r, &stubProvider{name: "zeta", available: true})
	mustRegister(t, r, &stubProvider{name: "alpha", available: true})
	mustRegister(t, r, &stubProvider{name: "broken", available: false})

	got := r.GetAvailable(context.Background())
	if len(got) != 2 {
		t.Fatalf("want 2 available providers, got %d", len(got))
	}
	if got[0].Name() != "alpha" || got[1].Name() != "zeta" {
		t.Fatalf("want [alpha, zeta] sorted, got [%s, %s]", got[0].Name(), got[1].Name())
	}
}

func TestRegistry_GetAvailable_Empty(t *testing.T) {
	r := NewRegistry()
	mustRegister(t, r, &stubProvider{name: "broken", available: false})
	got := r.GetAvailable(context.Background())
	if len(got) != 0 {
		t.Fatalf("want 0 available providers, got %d", len(got))
	}
}

func mustRegister(t *testing.T, r *Registry, p AgentProvider) {
	t.Helper()
	if err := r.Register(p, false); err != nil {
		t.Fatalf("register %s: %v", p.Name(), err)
	}
}
