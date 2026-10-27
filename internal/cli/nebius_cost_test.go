package cli

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/unleashtheagents/uta/internal/provider/builtin"
	"github.com/unleashtheagents/uta/internal/store"
)

func usageRow(model, role string, in, out int64, usd float64) store.EventRow {
	pl, _ := json.Marshal(map[string]any{
		"type": "usage", "model": model, "role": role,
		"tokens_in": in, "tokens_out": out, "reasoning_tokens": 7, "cached_tokens": 3, "usd": usd,
	})
	return store.EventRow{Kind: "subtask_stdout", Payload: pl}
}

func TestBuildNebiusCostReport(t *testing.T) {
	sess := store.Session{ID: "abcdef1234567890", Goal: "demo", Status: "completed"}
	ultraUSD, _ := builtin.NebiusEstimateUSD(nil, builtin.NebiusModelUltra, 1000, 500)
	superUSD, _ := builtin.NebiusEstimateUSD(nil, builtin.NebiusModelSuper, 8000, 2000)
	events := []store.EventRow{
		usageRow(builtin.NebiusModelUltra, "planner", 1000, 500, ultraUSD),
		usageRow(builtin.NebiusModelSuper, "worker", 4000, 1000, superUSD/2),
		usageRow(builtin.NebiusModelSuper, "worker", 4000, 1000, superUSD/2),
		{Kind: "sentinel_alert", Payload: json.RawMessage(`{"rule":"nemotron_sentinel_tool_block"}`)},
		{Kind: "subtask_tool_result", Payload: json.RawMessage(`{"sentinel":"pass"}`)},
		{Kind: "subtask_stdout", Payload: json.RawMessage(`{"type":"reasoning","text":"thinking"}`)},
		{Kind: "subtask_assistant_text", Payload: json.RawMessage(`{"text":"hi"}`)},
	}
	rep := buildNebiusCostReport(sess, events)
	if rep.Calls != 3 || rep.TokensIn != 9000 || rep.TokensOut != 2500 {
		t.Fatalf("totals = calls %d in %d out %d", rep.Calls, rep.TokensIn, rep.TokensOut)
	}
	if rep.ReasoningTokens != 21 || rep.CachedTokens != 9 {
		t.Errorf("reasoning/cached = %d/%d", rep.ReasoningTokens, rep.CachedTokens)
	}
	if rep.SentinelChecks != 2 || rep.SentinelBlocks != 1 {
		t.Errorf("sentinel checks/blocks = %d/%d", rep.SentinelChecks, rep.SentinelBlocks)
	}
	if len(rep.ByModel) != 2 || rep.ByModel[0].Key != builtin.NebiusModelSuper || rep.ByModel[0].Calls != 2 {
		t.Errorf("by model = %+v", rep.ByModel)
	}
	if len(rep.ByRole) != 2 {
		t.Errorf("by role = %+v", rep.ByRole)
	}
	// All-Ultra counterfactual: every call priced at Ultra rates.
	wantUltra, _ := builtin.NebiusEstimateUSD(nil, builtin.NebiusModelUltra, 9000, 2500)
	if diff := rep.AllUltraUSD - wantUltra; diff > 1e-9 || diff < -1e-9 {
		t.Errorf("all-ultra = %v, want %v", rep.AllUltraUSD, wantUltra)
	}
	if rep.SavingsPct <= 50 || rep.SavingsPct >= 100 {
		t.Errorf("savings = %.1f%%, expected a large saving when 90%% of tokens ran on Super", rep.SavingsPct)
	}

	cmd := &cobra.Command{}
	var sb strings.Builder
	cmd.SetOut(&sb)
	if err := renderNebiusCostReport(cmd, rep); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"super (nemotron-3-super-120b-a12b)", "ultra (Nemotron-3-Ultra-550b-a55b)", "90/10 saving:", "1 block(s)", "planner"} {
		if !strings.Contains(sb.String(), want) {
			t.Errorf("report missing %q:\n%s", want, sb.String())
		}
	}

	empty := buildNebiusCostReport(sess, nil)
	sb.Reset()
	_ = renderNebiusCostReport(cmd, empty)
	if !strings.Contains(sb.String(), "no Nebius usage events") {
		t.Errorf("empty report text: %s", sb.String())
	}
}
