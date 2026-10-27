package cli

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/unleashtheagents/uta/internal/provider/builtin"
	"github.com/unleashtheagents/uta/internal/store"
)

// nebiusUsageEvent mirrors the usage payload the Nebius provider emits at
// the end of every turn (a subtask_stdout event with type "usage").
type nebiusUsageEvent struct {
	Type            string  `json:"type"`
	Model           string  `json:"model"`
	Role            string  `json:"role"`
	TokensIn        int64   `json:"tokens_in"`
	TokensOut       int64   `json:"tokens_out"`
	ReasoningTokens int64   `json:"reasoning_tokens"`
	CachedTokens    int64   `json:"cached_tokens"`
	USD             float64 `json:"usd"`
}

// nebiusCostLine is one aggregated row of the cost report.
type nebiusCostLine struct {
	Key             string  `json:"key"`
	Calls           int     `json:"calls"`
	TokensIn        int64   `json:"tokens_in"`
	TokensOut       int64   `json:"tokens_out"`
	ReasoningTokens int64   `json:"reasoning_tokens"`
	CachedTokens    int64   `json:"cached_tokens"`
	USD             float64 `json:"usd"`
	Share           float64 `json:"share_of_tokens"`
}

// nebiusCostReport is the 90/10 economics view of one recorded session.
type nebiusCostReport struct {
	SessionID        string           `json:"session_id"`
	Goal             string           `json:"goal"`
	Status           string           `json:"status"`
	Calls            int              `json:"nebius_calls"`
	TokensIn         int64            `json:"tokens_in"`
	TokensOut        int64            `json:"tokens_out"`
	ReasoningTokens  int64            `json:"reasoning_tokens"`
	CachedTokens     int64            `json:"cached_tokens"`
	USD              float64          `json:"usd"`
	AllUltraUSD      float64          `json:"all_ultra_usd"`
	SavingsPct       float64          `json:"savings_vs_all_ultra_pct"`
	SentinelChecks   int              `json:"sentinel_checks"`
	SentinelBlocks   int              `json:"sentinel_blocks"`
	ByModel          []nebiusCostLine `json:"by_model"`
	ByRole           []nebiusCostLine `json:"by_role"`
	NonNebiusWorkers int              `json:"non_nebius_subtask_events"`
}

// buildNebiusCostReport folds a session's trajectory into per-model and
// per-role cost lines and computes the all-Ultra counterfactual: what the
// same tokens would have cost had every call run on Nemotron 3 Ultra.
func buildNebiusCostReport(sess store.Session, events []store.EventRow) nebiusCostReport {
	rep := nebiusCostReport{SessionID: sess.ID, Goal: sess.Goal, Status: sess.Status}
	byModel := map[string]*nebiusCostLine{}
	byRole := map[string]*nebiusCostLine{}
	add := func(m map[string]*nebiusCostLine, key string, u nebiusUsageEvent) {
		line, ok := m[key]
		if !ok {
			line = &nebiusCostLine{Key: key}
			m[key] = line
		}
		line.Calls++
		line.TokensIn += u.TokensIn
		line.TokensOut += u.TokensOut
		line.ReasoningTokens += u.ReasoningTokens
		line.CachedTokens += u.CachedTokens
		line.USD += u.USD
	}
	for _, ev := range events {
		switch ev.Kind {
		case "sentinel_alert":
			var pl struct {
				Rule string `json:"rule"`
			}
			_ = json.Unmarshal(ev.Payload, &pl)
			if pl.Rule == "nemotron_sentinel_tool_block" {
				rep.SentinelBlocks++
				rep.SentinelChecks++
			}
		case "subtask_tool_result":
			var pl struct {
				Sentinel string `json:"sentinel"`
			}
			_ = json.Unmarshal(ev.Payload, &pl)
			if pl.Sentinel == "pass" || pl.Sentinel == "error" {
				rep.SentinelChecks++
			}
		case "subtask_stdout":
			var u nebiusUsageEvent
			if err := json.Unmarshal(ev.Payload, &u); err != nil || u.Type != "usage" || u.Model == "" {
				continue
			}
			role := u.Role
			if role == "" {
				role = "worker"
			}
			rep.Calls++
			rep.TokensIn += u.TokensIn
			rep.TokensOut += u.TokensOut
			rep.ReasoningTokens += u.ReasoningTokens
			rep.CachedTokens += u.CachedTokens
			rep.USD += u.USD
			add(byModel, u.Model, u)
			add(byRole, role, u)
			if ultra, ok := builtin.NebiusEstimateUSD(nil, builtin.NebiusModelUltra, u.TokensIn, u.TokensOut); ok {
				rep.AllUltraUSD += ultra
			}
		}
	}
	total := float64(rep.TokensIn + rep.TokensOut)
	flatten := func(m map[string]*nebiusCostLine) []nebiusCostLine {
		out := make([]nebiusCostLine, 0, len(m))
		for _, l := range m {
			if total > 0 {
				l.Share = float64(l.TokensIn+l.TokensOut) / total * 100
			}
			out = append(out, *l)
		}
		sort.Slice(out, func(i, j int) bool {
			if out[i].USD != out[j].USD {
				return out[i].USD > out[j].USD
			}
			return out[i].Key < out[j].Key
		})
		return out
	}
	rep.ByModel = flatten(byModel)
	rep.ByRole = flatten(byRole)
	if rep.AllUltraUSD > 0 {
		rep.SavingsPct = (1 - rep.USD/rep.AllUltraUSD) * 100
	}
	return rep
}

func newNebiusCostCmd() *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "cost <session-id>",
		Short: "90/10 economics of one run: tokens and USD per Nemotron tier and role, vs an all-Ultra counterfactual",
		Long: `Fold a recorded session's Nebius usage events into a cost report.

Every Nebius turn records model, role, tokens (including reasoning and
prompt-cache hits) and list-price USD. The report groups them per Nemotron
tier and per orchestration role, counts sentinel checks and blocks, and
shows what the same tokens would have cost had every call run on
Nemotron 3 Ultra — the saving the 90/10 split actually delivered.`,
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: completeSessionIDs,
		RunE: func(cmd *cobra.Command, args []string) error {
			app, err := newApp(cmd.Context())
			if err != nil {
				return err
			}
			defer app.Close()
			id, err := app.Store.ResolveSessionID(args[0])
			if err != nil {
				return err
			}
			sess, err := app.Store.GetSession(id)
			if err != nil {
				if errors.Is(err, sql.ErrNoRows) {
					return fmt.Errorf("session not found: %s", id)
				}
				return err
			}
			events, err := app.Store.ListEvents(id, 0, 0)
			if err != nil {
				return err
			}
			rep := buildNebiusCostReport(sess, events)
			if asJSON {
				enc := json.NewEncoder(cmd.OutOrStdout())
				enc.SetIndent("", "  ")
				return enc.Encode(rep)
			}
			return renderNebiusCostReport(cmd, rep)
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit the cost report as JSON")
	return cmd
}

func renderNebiusCostReport(cmd *cobra.Command, rep nebiusCostReport) error {
	out := cmd.OutOrStdout()
	short := rep.SessionID
	if len(short) > 8 {
		short = short[:8]
	}
	goal := rep.Goal
	if len(goal) > 70 {
		goal = goal[:70] + "…"
	}
	fmt.Fprintf(out, "Nebius × Nemotron cost report  session=%s  status=%s\n  goal: %s\n\n", short, rep.Status, goal)
	if rep.Calls == 0 {
		fmt.Fprintln(out, "no Nebius usage events recorded for this session (was it run with --worker nebius on uta >= 0.14?)")
		return nil
	}
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "MODEL\tCALLS\tTOKENS_IN\tTOKENS_OUT\tREASONING\tCACHED\tSHARE\tUSD")
	for _, l := range rep.ByModel {
		fmt.Fprintf(tw, "%s\t%d\t%d\t%d\t%d\t%d\t%.1f%%\t$%.4f\n", shortNebiusModel(l.Key), l.Calls, l.TokensIn, l.TokensOut, l.ReasoningTokens, l.CachedTokens, l.Share, l.USD)
	}
	_ = tw.Flush()
	fmt.Fprintln(out)
	tw = tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ROLE\tCALLS\tTOKENS\tSHARE\tUSD")
	for _, l := range rep.ByRole {
		fmt.Fprintf(tw, "%s\t%d\t%d\t%.1f%%\t$%.4f\n", l.Key, l.Calls, l.TokensIn+l.TokensOut, l.Share, l.USD)
	}
	_ = tw.Flush()
	fmt.Fprintln(out)
	fmt.Fprintf(out, "  total:          %d call(s), %d tokens (%d reasoning, %d cached), $%.4f\n",
		rep.Calls, rep.TokensIn+rep.TokensOut, rep.ReasoningTokens, rep.CachedTokens, rep.USD)
	fmt.Fprintf(out, "  all-Ultra:      $%.4f for the same tokens on Nemotron 3 Ultra\n", rep.AllUltraUSD)
	fmt.Fprintf(out, "  90/10 saving:   %.1f%%\n", rep.SavingsPct)
	fmt.Fprintf(out, "  sentinel:       %d check(s), %d block(s)\n", rep.SentinelChecks, rep.SentinelBlocks)
	return nil
}

// shortNebiusModel renders a Nemotron model ID as its tier alias when known.
func shortNebiusModel(id string) string {
	for _, m := range builtin.NebiusModelCatalog() {
		if m.ID == id {
			return m.Alias + " (" + strings.TrimPrefix(id, "nvidia/") + ")"
		}
	}
	return id
}
