package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/unleashtheagents/uta/internal/engine"
	"github.com/unleashtheagents/uta/internal/limits"
	"github.com/unleashtheagents/uta/internal/paths"
)

// usageGuard adapts limits.Guard to the engine's UsageHook.
type usageGuard struct{ *limits.Guard }

func (g usageGuard) BeforeCall(ctx context.Context, name string) error {
	return g.Guard.BeforeCall(ctx, name)
}

func (g usageGuard) AfterCall(ctx context.Context, rec engine.UsageRecord) {
	g.Guard.AfterCall(ctx, limits.Entry{
		Ts: rec.Ts, Provider: rec.Provider, Role: string(rec.Role), SessionID: rec.SessionID,
		TokensIn: rec.TokensIn, TokensOut: rec.TokensOut, USDCents: rec.USDCents,
	})
}

// loadUsageGuard builds the spend guard from ~/.uta/limits.yaml and the
// global usage ledger. A malformed limits file is reported on stderr and
// treated as empty so a typo never blocks work; the ledger still records.
func loadUsageGuard(globalHome string, errOut io.Writer) usageGuard {
	cfg, err := limits.Load(limits.File(globalHome))
	if err != nil && errOut != nil {
		fmt.Fprintln(errOut, "warn: spend limits:", err)
	}
	return usageGuard{&limits.Guard{Config: cfg, Ledger: &limits.Ledger{Path: limits.LedgerFile(globalHome)}}}
}

func writeJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

func newLimitsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "limits",
		Short: "set and inspect per-provider spend caps (daily / monthly USD and tokens)",
		Long: `Cap what each provider may spend across every project on this machine.
Caps live in ~/.uta/limits.yaml; spend is tallied from ~/.uta/usage/ledger.jsonl,
which every provider call appends to. When a provider reaches a cap, the
next call is refused with a quota error (steer missions fail over to the
next provider in an any(...) set; uta run reports the limit) until the
window rolls over or the cap is raised.

  uta limits set nebius --daily-usd 5 --monthly-usd 50
  uta limits set claude --monthly-tokens 20000000
  uta limits status
  uta limits clear nebius

USD figures use each provider's reported cost (Nebius list prices per
Nemotron tier, Claude's own cost field, operator rates for gemini / agy).`,
	}

	var dailyUSD, monthlyUSD string
	var dailyTokens, monthlyTokens int64
	setCmd := &cobra.Command{
		Use:   "set <provider>",
		Short: "set caps for a provider (unspecified caps are kept)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			home, err := paths.Home()
			if err != nil {
				return err
			}
			path := limits.File(home)
			cfg, err := limits.Load(path)
			if err != nil {
				return err
			}
			name := strings.ToLower(strings.TrimSpace(args[0]))
			lim := cfg.Providers[name]
			changed := false
			if cmd.Flags().Changed("daily-usd") {
				if lim.DailyUSD, err = limits.ParseUSD(dailyUSD); err != nil {
					return err
				}
				changed = true
			}
			if cmd.Flags().Changed("monthly-usd") {
				if lim.MonthlyUSD, err = limits.ParseUSD(monthlyUSD); err != nil {
					return err
				}
				changed = true
			}
			if cmd.Flags().Changed("daily-tokens") {
				if dailyTokens < 0 {
					return fmt.Errorf("--daily-tokens must be >= 0")
				}
				lim.DailyTokens = dailyTokens
				changed = true
			}
			if cmd.Flags().Changed("monthly-tokens") {
				if monthlyTokens < 0 {
					return fmt.Errorf("--monthly-tokens must be >= 0")
				}
				lim.MonthlyTokens = monthlyTokens
				changed = true
			}
			if !changed {
				return fmt.Errorf("nothing to set: pass at least one of --daily-usd, --monthly-usd, --daily-tokens, --monthly-tokens")
			}
			if lim.IsZero() {
				delete(cfg.Providers, name)
			} else {
				cfg.Providers[name] = lim
			}
			if err := limits.Save(path, cfg); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s: %s (saved to %s)\n", name, describeLimit(lim), path)
			return nil
		},
	}
	setCmd.Flags().StringVar(&dailyUSD, "daily-usd", "", "max USD per UTC day (0 = uncapped)")
	setCmd.Flags().StringVar(&monthlyUSD, "monthly-usd", "", "max USD per UTC calendar month (0 = uncapped)")
	setCmd.Flags().Int64Var(&dailyTokens, "daily-tokens", 0, "max tokens (in + out) per UTC day (0 = uncapped)")
	setCmd.Flags().Int64Var(&monthlyTokens, "monthly-tokens", 0, "max tokens (in + out) per UTC calendar month (0 = uncapped)")

	clearCmd := &cobra.Command{
		Use:   "clear <provider>",
		Short: "remove every cap for a provider",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			home, err := paths.Home()
			if err != nil {
				return err
			}
			path := limits.File(home)
			cfg, err := limits.Load(path)
			if err != nil {
				return err
			}
			name := strings.ToLower(strings.TrimSpace(args[0]))
			if _, ok := cfg.Providers[name]; !ok {
				fmt.Fprintf(cmd.OutOrStdout(), "%s: no caps set\n", name)
				return nil
			}
			delete(cfg.Providers, name)
			if err := limits.Save(path, cfg); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s: caps removed\n", name)
			return nil
		},
	}

	var statusJSON bool
	statusCmd := &cobra.Command{
		Use:   "status",
		Short: "caps versus spend today and this month, per provider",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			home, err := paths.Home()
			if err != nil {
				return err
			}
			g := loadUsageGuard(home, cmd.ErrOrStderr())
			names := map[string]bool{}
			for n := range g.Config.Providers {
				names[n] = true
			}
			if provs, err := g.Ledger.Providers(); err == nil {
				for _, n := range provs {
					names[n] = true
				}
			}
			var sorted []string
			for n := range names {
				sorted = append(sorted, n)
			}
			sort.Strings(sorted)
			var rows []limits.Status
			for _, n := range sorted {
				st, err := g.StatusFor(n)
				if err != nil {
					return err
				}
				rows = append(rows, st)
			}
			if statusJSON {
				return writeJSON(cmd.OutOrStdout(), map[string]any{
					"limits_file": limits.File(home), "ledger": limits.LedgerFile(home), "providers": rows,
				})
			}
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "limits: %s\nledger: %s\nwindows: UTC day and calendar month (now %s)\n\n", limits.File(home), limits.LedgerFile(home), time.Now().UTC().Format(time.RFC3339))
			if len(rows) == 0 {
				fmt.Fprintln(out, "no caps set and no usage recorded yet. Try: uta limits set nebius --daily-usd 5")
				return nil
			}
			tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, "PROVIDER\tTODAY\tTHIS MONTH\tCAPS\tSTATE")
			for _, r := range rows {
				state := "ok"
				if why := r.Exceeded(); why != "" {
					state = "BLOCKED: " + why
				} else if r.Limit.IsZero() {
					state = "uncapped"
				}
				fmt.Fprintf(tw, "%s\t$%.4f / %d tok / %d calls\t$%.4f / %d tok / %d calls\t%s\t%s\n",
					r.Provider,
					float64(r.Day.USDCents)/100, r.Day.Tokens, r.Day.Calls,
					float64(r.Month.USDCents)/100, r.Month.Tokens, r.Month.Calls,
					describeLimit(r.Limit), state)
			}
			return tw.Flush()
		},
	}
	statusCmd.Flags().BoolVar(&statusJSON, "json", false, "emit as JSON")

	cmd.AddCommand(setCmd, clearCmd, statusCmd)
	return cmd
}

func describeLimit(l limits.ProviderLimit) string {
	if l.IsZero() {
		return "none"
	}
	var parts []string
	if l.DailyUSD > 0 {
		parts = append(parts, fmt.Sprintf("$%.2f/day", l.DailyUSD))
	}
	if l.MonthlyUSD > 0 {
		parts = append(parts, fmt.Sprintf("$%.2f/month", l.MonthlyUSD))
	}
	if l.DailyTokens > 0 {
		parts = append(parts, fmt.Sprintf("%d tok/day", l.DailyTokens))
	}
	if l.MonthlyTokens > 0 {
		parts = append(parts, fmt.Sprintf("%d tok/month", l.MonthlyTokens))
	}
	return strings.Join(parts, ", ")
}
