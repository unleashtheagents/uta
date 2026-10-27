// Package limits implements user-level spend limits for agent providers and
// the usage ledger that enforces them.
//
// Two files under the global uta home (~/.uta) back it:
//
//	limits.yaml          per-provider caps (daily / monthly USD and tokens)
//	usage/ledger.jsonl   one line per provider call: when, who, how much
//
// The engine consults a Guard before every provider call and records the
// outcome after it. Limits are global per user, not per project, because
// an API key is billed per user: a cap of $5/day on nebius holds across
// every repository the same machine runs uta in.
package limits

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/unleashtheagents/uta/internal/provider"
)

// ErrExceeded marks a call refused by a spend limit. It wraps
// provider.ErrQuota so existing quota handling (failover, retry policy)
// treats it the same way as an upstream 429.
var ErrExceeded = fmt.Errorf("spend limit exceeded: %w", provider.ErrQuota)

// ProviderLimit is the cap set for one provider. Zero means uncapped.
type ProviderLimit struct {
	DailyUSD      float64 `yaml:"daily_usd,omitempty" json:"daily_usd,omitempty"`
	MonthlyUSD    float64 `yaml:"monthly_usd,omitempty" json:"monthly_usd,omitempty"`
	DailyTokens   int64   `yaml:"daily_tokens,omitempty" json:"daily_tokens,omitempty"`
	MonthlyTokens int64   `yaml:"monthly_tokens,omitempty" json:"monthly_tokens,omitempty"`
}

// IsZero reports whether no cap is set.
func (p ProviderLimit) IsZero() bool {
	return p.DailyUSD == 0 && p.MonthlyUSD == 0 && p.DailyTokens == 0 && p.MonthlyTokens == 0
}

// Config is the on-disk shape of limits.yaml.
type Config struct {
	Providers map[string]ProviderLimit `yaml:"providers" json:"providers"`
}

// File returns the limits.yaml path inside the given uta home.
func File(home string) string { return filepath.Join(home, "limits.yaml") }

// LedgerFile returns the usage ledger path inside the given uta home.
func LedgerFile(home string) string { return filepath.Join(home, "usage", "ledger.jsonl") }

// Load reads limits.yaml. A missing file yields an empty config, not an error.
func Load(path string) (Config, error) {
	cfg := Config{Providers: map[string]ProviderLimit{}}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return cfg, nil
	}
	if err != nil {
		return cfg, err
	}
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return cfg, fmt.Errorf("parse %s: %w", path, err)
	}
	if cfg.Providers == nil {
		cfg.Providers = map[string]ProviderLimit{}
	}
	for name, p := range cfg.Providers {
		if p.DailyUSD < 0 || p.MonthlyUSD < 0 || p.DailyTokens < 0 || p.MonthlyTokens < 0 {
			return cfg, fmt.Errorf("%s: provider %q has a negative limit", path, name)
		}
	}
	return cfg, nil
}

// Save writes limits.yaml with 0600 permissions.
func Save(path string, cfg Config) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := yaml.Marshal(cfg)
	if err != nil {
		return err
	}
	header := "# uta spend limits — per-provider caps enforced before every provider call.\n# Manage with `uta limits set|clear|status`. Zero or absent means uncapped.\n"
	return os.WriteFile(path, append([]byte(header), data...), 0o600)
}

// Entry is one ledger line.
type Entry struct {
	Ts        time.Time `json:"ts"`
	Provider  string    `json:"provider"`
	Model     string    `json:"model,omitempty"`
	Role      string    `json:"role,omitempty"`
	SessionID string    `json:"session_id,omitempty"`
	TokensIn  int64     `json:"tokens_in"`
	TokensOut int64     `json:"tokens_out"`
	USDCents  int64     `json:"usd_cents"`
}

// Spend is an aggregate over a window.
type Spend struct {
	Calls    int
	Tokens   int64
	USDCents int64
}

// Ledger is an append-only JSONL usage log. Appends are single write
// syscalls under O_APPEND, so concurrent uta processes interleave whole
// lines; reads tolerate a torn trailing line.
type Ledger struct {
	Path string
	mu   sync.Mutex
}

// Record appends one entry.
func (l *Ledger) Record(e Entry) error {
	if l == nil || l.Path == "" {
		return nil
	}
	if e.Ts.IsZero() {
		e.Ts = time.Now()
	}
	line, err := json.Marshal(e)
	if err != nil {
		return err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := os.MkdirAll(filepath.Dir(l.Path), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(l.Path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(append(line, '\n'))
	return err
}

// SpendSince sums the ledger for one provider from `since` onward. An empty
// provider sums every provider.
func (l *Ledger) SpendSince(providerName string, since time.Time) (Spend, error) {
	var sp Spend
	if l == nil || l.Path == "" {
		return sp, nil
	}
	f, err := os.Open(l.Path)
	if errors.Is(err, os.ErrNotExist) {
		return sp, nil
	}
	if err != nil {
		return sp, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		var e Entry
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			continue // torn or foreign line
		}
		if providerName != "" && e.Provider != providerName {
			continue
		}
		if e.Ts.Before(since) {
			continue
		}
		sp.Calls++
		sp.Tokens += e.TokensIn + e.TokensOut
		sp.USDCents += e.USDCents
	}
	return sp, sc.Err()
}

// Providers lists every provider name that appears in the ledger.
func (l *Ledger) Providers() ([]string, error) {
	seen := map[string]bool{}
	if l == nil || l.Path == "" {
		return nil, nil
	}
	f, err := os.Open(l.Path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		var e Entry
		if json.Unmarshal(sc.Bytes(), &e) == nil && e.Provider != "" {
			seen[e.Provider] = true
		}
	}
	out := make([]string, 0, len(seen))
	for k := range seen {
		out = append(out, k)
	}
	sort.Strings(out)
	return out, sc.Err()
}

// Windows returns the start of the current UTC day and the current UTC month.
func Windows(now time.Time) (dayStart, monthStart time.Time) {
	now = now.UTC()
	dayStart = time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	monthStart = time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	return dayStart, monthStart
}

// Guard enforces a Config against a Ledger. It satisfies the engine's usage
// hook: BeforeCall refuses a call once the provider's spend is at or over a
// cap; AfterCall records what the call used.
type Guard struct {
	Config Config
	Ledger *Ledger
	Now    func() time.Time
}

// Status is one provider's caps versus its spend so far.
type Status struct {
	Provider string
	Limit    ProviderLimit
	Day      Spend
	Month    Spend
}

// Exceeded reports which cap, if any, the status is at or over.
func (s Status) Exceeded() string {
	switch {
	case s.Limit.DailyUSD > 0 && float64(s.Day.USDCents)/100 >= s.Limit.DailyUSD:
		return fmt.Sprintf("daily USD cap $%.2f reached ($%.2f spent today)", s.Limit.DailyUSD, float64(s.Day.USDCents)/100)
	case s.Limit.MonthlyUSD > 0 && float64(s.Month.USDCents)/100 >= s.Limit.MonthlyUSD:
		return fmt.Sprintf("monthly USD cap $%.2f reached ($%.2f spent this month)", s.Limit.MonthlyUSD, float64(s.Month.USDCents)/100)
	case s.Limit.DailyTokens > 0 && s.Day.Tokens >= s.Limit.DailyTokens:
		return fmt.Sprintf("daily token cap %d reached (%d used today)", s.Limit.DailyTokens, s.Day.Tokens)
	case s.Limit.MonthlyTokens > 0 && s.Month.Tokens >= s.Limit.MonthlyTokens:
		return fmt.Sprintf("monthly token cap %d reached (%d used this month)", s.Limit.MonthlyTokens, s.Month.Tokens)
	}
	return ""
}

func (g *Guard) now() time.Time {
	if g.Now != nil {
		return g.Now()
	}
	return time.Now()
}

// StatusFor computes caps versus spend for one provider.
func (g *Guard) StatusFor(providerName string) (Status, error) {
	st := Status{Provider: providerName, Limit: g.Config.Providers[providerName]}
	day, month := Windows(g.now())
	var err error
	if st.Day, err = g.Ledger.SpendSince(providerName, day); err != nil {
		return st, err
	}
	if st.Month, err = g.Ledger.SpendSince(providerName, month); err != nil {
		return st, err
	}
	return st, nil
}

// BeforeCall refuses the call with ErrExceeded when the provider is at or
// over any configured cap. Providers without a cap always pass.
func (g *Guard) BeforeCall(_ context.Context, providerName string) error {
	if g == nil {
		return nil
	}
	lim, ok := g.Config.Providers[providerName]
	if !ok || lim.IsZero() {
		return nil
	}
	st, err := g.StatusFor(providerName)
	if err != nil {
		return nil // a broken ledger must not block work; status surfaces it
	}
	if why := st.Exceeded(); why != "" {
		return fmt.Errorf("%s: %s (raise with `uta limits set %s ...`): %w", providerName, why, providerName, ErrExceeded)
	}
	return nil
}

// AfterCall records the call's usage.
func (g *Guard) AfterCall(_ context.Context, e Entry) {
	if g == nil || g.Ledger == nil {
		return
	}
	_ = g.Ledger.Record(e)
}

// ParseUSD accepts "5", "5.50", "$5" for CLI flags.
func ParseUSD(s string) (float64, error) {
	s = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(s), "$"))
	var v float64
	if _, err := fmt.Sscanf(s, "%g", &v); err != nil || v < 0 {
		return 0, fmt.Errorf("invalid USD amount %q", s)
	}
	return v, nil
}
