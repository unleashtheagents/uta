package builtin

import (
	"math"
	"testing"
)

func TestNebiusListPricesEnforceDollarBudgets(t *testing.T) {
	// 10k in / 2k out on Ultra: 10k*$1/M + 2k*$3/M = $0.010 + $0.006 = $0.016 -> 2 cents (ceiling).
	if got := nebiusEstimateUSDCents(nil, NebiusModelUltra, 10_000, 2_000); got != 2 {
		t.Errorf("ultra cents = %d, want 2", got)
	}
	usd, ok := NebiusEstimateUSD(nil, "super", 1_000_000, 1_000_000)
	if !ok || math.Abs(usd-1.20) > 1e-9 {
		t.Errorf("super 1M/1M = %v ok=%v, want 1.20", usd, ok)
	}
	// Tiny calls are not charged a cent per dimension: 100 in / 50 out on Nano is ~$0.000018 -> 1 cent total.
	if got := nebiusEstimateUSDCents(nil, NebiusModelNano, 100, 50); got != 1 {
		t.Errorf("nano tiny call cents = %d, want 1", got)
	}
	// Unknown model: no price, no charge.
	if got := nebiusEstimateUSDCents(nil, "custom/model", 1000, 1000); got != 0 {
		t.Errorf("unknown model cents = %d, want 0", got)
	}
	// Disabled pricing.
	if got := nebiusEstimateUSDCents([]string{"UTA_NEBIUS_PRICING=off"}, NebiusModelUltra, 1e6, 1e6); got != 0 {
		t.Errorf("pricing off cents = %d, want 0", got)
	}
	// Env override wins: 1 cent per ktok in, 2 cents per ktok out.
	t.Setenv("UTA_NEBIUS_INPUT_CENTS_PER_KTOK", "1")
	t.Setenv("UTA_NEBIUS_OUTPUT_CENTS_PER_KTOK", "2")
	if got := nebiusEstimateUSDCents(nil, NebiusModelNano, 1000, 1000); got != 3 {
		t.Errorf("env override cents = %d, want 3", got)
	}
	// Super-VL is flagged as an assumed price.
	if lp, ok := NebiusListPriceFor("vl"); !ok || !lp.Assumed {
		t.Errorf("vl price should be present and marked assumed: %+v ok=%v", lp, ok)
	}
}
