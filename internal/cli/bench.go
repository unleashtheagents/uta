package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	"github.com/unleashtheagents/uta/internal/provider"
	"github.com/unleashtheagents/uta/internal/provider/builtin"
)

type benchResult struct {
	Provider   string        `json:"provider"`
	Available  bool          `json:"available"`
	Duration   time.Duration `json:"duration"`
	DurationMs int64         `json:"duration_ms"`
	TokensIn   int64         `json:"tokens_in"`
	TokensOut  int64         `json:"tokens_out"`
	TokPerSec  float64       `json:"tok_per_sec"`
	USDCents   int64         `json:"usd_cents"`
	USD        float64       `json:"usd,omitempty"` // precise estimate where the provider has list prices (nebius)
	Status     string        `json:"status"`
	Err        string        `json:"error,omitempty"`
}

func newBenchCmd() *cobra.Command {
	var (
		prompt  string
		timeout time.Duration
		asJSON  bool
	)
	cmd := &cobra.Command{
		Use:   "bench",
		Short: "benchmark latency, throughput (tok/s), and response quality across installed providers",
		Long: `Run a standardized benchmark prompt across all installed and available agent providers.
Measures wall-clock latency, token output throughput (tokens/sec), and execution status,
surfacing a side-by-side comparative table to help choose optimal planner and worker assignments.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			app, err := newApp(cmd.Context())
			if err != nil {
				return err
			}
			defer app.Close()

			detections := app.Registry.DetectAll(cmd.Context())
			available := app.Registry.GetAvailable(cmd.Context())

			if len(available) == 0 {
				fmt.Fprintln(cmd.OutOrStdout(), "no available providers detected on PATH")
				return nil
			}

			fmt.Fprintf(cmd.ErrOrStderr(), "▶ Running provider benchmark (%d provider(s)) · timeout=%s\n", len(available), timeout)

			var results []benchResult
			for _, p := range available {
				pName := p.Name()
				det := detections[pName]

				fmt.Fprintf(cmd.ErrOrStderr(), "  testing %-12s ... ", pName)
				start := time.Now()

				bCtx, cancel := context.WithTimeout(cmd.Context(), timeout)
				res, rErr := p.RunHeadless(bCtx, prompt, provider.RunOptions{}, nil)
				cancel()
				dur := time.Since(start)

				br := benchResult{
					Provider:   pName,
					Available:  det.Available,
					Duration:   dur,
					DurationMs: dur.Milliseconds(),
					TokensIn:   res.TokensIn,
					TokensOut:  res.TokensOut,
					USDCents:   res.ApproxUSDCents,
				}
				if pName == "nebius" {
					model := builtin.ResolveNebiusModel(os.Getenv("NEBIUS_MODEL"))
					if os.Getenv("NEBIUS_MODEL") == "" {
						model = builtin.NebiusModelForRole(provider.RoleWorker)
					}
					if usd, ok := builtin.NebiusEstimateUSD(nil, model, res.TokensIn, res.TokensOut); ok {
						br.USD = usd
					}
				} else if res.ApproxUSDCents > 0 {
					br.USD = float64(res.ApproxUSDCents) / 100
				}

				if rErr != nil {
					br.Status = "error"
					br.Err = rErr.Error()
					fmt.Fprintf(cmd.ErrOrStderr(), "✖ failed (%v)\n", rErr)
				} else {
					br.Status = "ok"
					if br.TokensOut > 0 && dur.Seconds() > 0 {
						br.TokPerSec = float64(br.TokensOut) / dur.Seconds()
					}
					fmt.Fprintf(cmd.ErrOrStderr(), "✓ ok (%s)\n", dur.Round(time.Millisecond))
				}

				results = append(results, br)
			}

			if asJSON {
				enc := json.NewEncoder(cmd.OutOrStdout())
				enc.SetIndent("", "  ")
				return enc.Encode(results)
			}

			fmt.Fprintln(cmd.OutOrStdout())
			tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, "PROVIDER\tSTATUS\tDURATION\tTOKENS_IN\tTOKENS_OUT\tTHROUGHPUT\tCOST")
			for _, r := range results {
				tpStr := "-"
				if r.TokPerSec > 0 {
					tpStr = fmt.Sprintf("%.1f tok/s", r.TokPerSec)
				}
				costStr := "-"
				if r.USD > 0 {
					costStr = fmt.Sprintf("$%.4f", r.USD)
				}
				fmt.Fprintf(tw, "%s\t%s\t%s\t%d\t%d\t%s\t%s\n",
					r.Provider, r.Status, r.Duration.Round(time.Millisecond),
					r.TokensIn, r.TokensOut, tpStr, costStr)
			}
			return tw.Flush()
		},
	}
	cmd.Flags().StringVarP(&prompt, "prompt", "p", "Explain how a B-tree index works in 2 concise paragraphs.", "benchmark prompt to send to each provider")
	cmd.Flags().DurationVar(&timeout, "timeout", 30*time.Second, "per-provider benchmark timeout")
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit JSON benchmark report")
	return cmd
}
