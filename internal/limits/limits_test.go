package limits

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/unleashtheagents/uta/internal/provider"
)

func TestConfigRoundTripAndMissingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "limits.yaml")
	cfg, err := Load(path)
	if err != nil || len(cfg.Providers) != 0 {
		t.Fatalf("missing file should load empty: %+v %v", cfg, err)
	}
	cfg.Providers["nebius"] = ProviderLimit{DailyUSD: 5, MonthlyTokens: 2_000_000}
	if err := Save(path, cfg); err != nil {
		t.Fatal(err)
	}
	back, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if back.Providers["nebius"].DailyUSD != 5 || back.Providers["nebius"].MonthlyTokens != 2_000_000 {
		t.Errorf("round trip lost data: %+v", back.Providers["nebius"])
	}
}

func TestLedgerSpendWindows(t *testing.T) {
	l := &Ledger{Path: filepath.Join(t.TempDir(), "usage", "ledger.jsonl")}
	now := time.Date(2026, 10, 27, 15, 0, 0, 0, time.UTC)
	entries := []Entry{
		{Ts: now.Add(-1 * time.Hour), Provider: "nebius", TokensIn: 1000, TokensOut: 500, USDCents: 2},
		{Ts: now.Add(-30 * time.Hour), Provider: "nebius", TokensIn: 100, TokensOut: 50, USDCents: 1},   // yesterday, same month
		{Ts: now.Add(-40 * 24 * time.Hour), Provider: "nebius", TokensIn: 9, TokensOut: 9, USDCents: 9}, // last month
		{Ts: now.Add(-1 * time.Hour), Provider: "claude", TokensIn: 7, TokensOut: 7, USDCents: 70},
	}
	for _, e := range entries {
		if err := l.Record(e); err != nil {
			t.Fatal(err)
		}
	}
	day, month := Windows(now)
	d, err := l.SpendSince("nebius", day)
	if err != nil || d.Calls != 1 || d.Tokens != 1500 || d.USDCents != 2 {
		t.Errorf("day spend = %+v err=%v", d, err)
	}
	m, _ := l.SpendSince("nebius", month)
	if m.Calls != 2 || m.Tokens != 1650 || m.USDCents != 3 {
		t.Errorf("month spend = %+v", m)
	}
	all, _ := l.SpendSince("", month)
	if all.Calls != 3 || all.USDCents != 73 {
		t.Errorf("all providers = %+v", all)
	}
	provs, _ := l.Providers()
	if len(provs) != 2 || provs[0] != "claude" || provs[1] != "nebius" {
		t.Errorf("providers = %v", provs)
	}
	// Missing ledger reads as zero.
	empty := &Ledger{Path: filepath.Join(t.TempDir(), "none.jsonl")}
	if sp, err := empty.SpendSince("nebius", day); err != nil || sp.Calls != 0 {
		t.Errorf("missing ledger: %+v %v", sp, err)
	}
}

func TestGuardBlocksAtCap(t *testing.T) {
	now := time.Date(2026, 10, 27, 15, 0, 0, 0, time.UTC)
	g := &Guard{
		Config: Config{Providers: map[string]ProviderLimit{"nebius": {DailyUSD: 0.05}}},
		Ledger: &Ledger{Path: filepath.Join(t.TempDir(), "ledger.jsonl")},
		Now:    func() time.Time { return now },
	}
	ctx := context.Background()
	if err := g.BeforeCall(ctx, "nebius"); err != nil {
		t.Fatalf("fresh ledger should pass: %v", err)
	}
	if err := g.BeforeCall(ctx, "claude"); err != nil {
		t.Fatalf("uncapped provider should pass: %v", err)
	}
	g.AfterCall(ctx, Entry{Ts: now, Provider: "nebius", TokensIn: 10, TokensOut: 10, USDCents: 4})
	if err := g.BeforeCall(ctx, "nebius"); err != nil {
		t.Fatalf("under cap should pass: %v", err)
	}
	g.AfterCall(ctx, Entry{Ts: now, Provider: "nebius", TokensIn: 10, TokensOut: 10, USDCents: 1})
	err := g.BeforeCall(ctx, "nebius")
	if err == nil || !errors.Is(err, ErrExceeded) || !errors.Is(err, provider.ErrQuota) {
		t.Fatalf("at cap should refuse with ErrExceeded wrapping ErrQuota, got %v", err)
	}
	st, _ := g.StatusFor("nebius")
	if st.Exceeded() == "" || st.Day.USDCents != 5 {
		t.Errorf("status = %+v", st)
	}
	// Token caps work the same way.
	g.Config.Providers["nebius"] = ProviderLimit{DailyTokens: 30}
	if err := g.BeforeCall(ctx, "nebius"); err == nil {
		t.Errorf("40 tokens used against a 30 token cap should refuse")
	}
	// A nil guard is inert.
	var none *Guard
	if err := none.BeforeCall(ctx, "nebius"); err != nil {
		t.Errorf("nil guard: %v", err)
	}
}

func TestParseUSD(t *testing.T) {
	for in, want := range map[string]float64{"5": 5, "$5.50": 5.5, " 0.25 ": 0.25} {
		got, err := ParseUSD(in)
		if err != nil || got != want {
			t.Errorf("ParseUSD(%q) = %v, %v", in, got, err)
		}
	}
	if _, err := ParseUSD("-1"); err == nil {
		t.Error("negative should fail")
	}
	if _, err := ParseUSD("abc"); err == nil {
		t.Error("garbage should fail")
	}
}
