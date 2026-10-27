package engine

import (
	"context"
	"errors"
	"testing"

	"github.com/unleashtheagents/uta/internal/provider"
)

type recordingHook struct {
	refuse  error
	before  []string
	records []UsageRecord
}

func (h *recordingHook) BeforeCall(_ context.Context, name string) error {
	h.before = append(h.before, name)
	return h.refuse
}

func (h *recordingHook) AfterCall(_ context.Context, rec UsageRecord) {
	h.records = append(h.records, rec)
}

type usageStubProvider struct{}

func (usageStubProvider) Name() string { return "stub" }
func (usageStubProvider) Detect(context.Context) provider.Detection {
	return provider.Detection{Available: true}
}
func (usageStubProvider) RunHeadless(_ context.Context, _ string, _ provider.RunOptions, _ chan<- provider.Event) (provider.RunResult, error) {
	return provider.RunResult{FinalText: "ok", TokensIn: 11, TokensOut: 5, ApproxUSDCents: 3}, nil
}

func TestCallProviderConsultsUsageHook(t *testing.T) {
	hook := &recordingHook{}
	deps := newTestDeps(t)
	deps.Usage = hook
	s := New(deps)
	ctx := withRole(context.Background(), provider.RolePlanner)
	res, err := s.callProvider(ctx, "sess", "sub", usageStubProvider{}, "hi", RunRequest{})
	if err != nil || res.FinalText != "ok" {
		t.Fatalf("call: %v %+v", err, res)
	}
	if len(hook.before) != 1 || hook.before[0] != "stub" {
		t.Errorf("BeforeCall not consulted: %v", hook.before)
	}
	if len(hook.records) != 1 {
		t.Fatalf("AfterCall records = %d", len(hook.records))
	}
	rec := hook.records[0]
	if rec.Provider != "stub" || rec.Role != provider.RolePlanner || rec.TokensIn != 11 || rec.TokensOut != 5 || rec.USDCents != 3 || rec.SessionID != "sess" {
		t.Errorf("record = %+v", rec)
	}

	refusing := &recordingHook{refuse: errors.New("capped: " + provider.ErrQuota.Error())}
	deps.Usage = refusing
	s = New(deps)
	if _, err := s.callProvider(ctx, "sess", "sub", usageStubProvider{}, "hi", RunRequest{}); err == nil {
		t.Fatal("refused BeforeCall must abort the call")
	}
	if len(refusing.records) != 0 {
		t.Errorf("no usage should be recorded for a refused call")
	}
}
