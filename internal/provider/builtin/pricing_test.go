package builtin

import "testing"

func TestEstimateUSDCents_NoRatesReturnsZero(t *testing.T) {
	// When the operator hasn't configured pricing, the USD field must be
	// zero — otherwise a profile-level dollar budget would trip on a
	// non-existent cost the moment a single token flowed.
	if got := estimateUSDCents(1_000_000, 1_000_000, 0, 0); got != 0 {
		t.Fatalf("estimateUSDCents(_, _, 0, 0) = %d, want 0", got)
	}
}

func TestEstimateUSDCents_BothRatesApplied(t *testing.T) {
	// 1000 input × 30¢/Ktok + 500 output × 60¢/Ktok = 30 + 30 = 60¢.
	if got := estimateUSDCents(1000, 500, 30, 60); got != 60 {
		t.Fatalf("estimateUSDCents(1000, 500, 30, 60) = %d, want 60", got)
	}
}

func TestEstimateUSDCents_CeilingRoundsUp(t *testing.T) {
	// 1 token × 30¢/Ktok = 30/1000 = 0.03¢ — must round UP to 1¢ so the
	// budget gate treats the estimate as a pessimistic ceiling rather than
	// silently dropping sub-cent fractions across many small calls.
	if got := estimateUSDCents(1, 0, 30, 0); got != 1 {
		t.Fatalf("estimateUSDCents(1, 0, 30, 0) = %d, want 1 (ceiling)", got)
	}
}

func TestEstimateUSDCents_OneDimensionDisabled(t *testing.T) {
	// Operators may price input only (some Gemini tiers). Verify a zero
	// output rate contributes nothing without disabling the input charge.
	if got := estimateUSDCents(1000, 1_000_000, 50, 0); got != 50 {
		t.Fatalf("estimateUSDCents(1000, 1m, 50, 0) = %d, want 50 (output dimension disabled)", got)
	}
}

func TestEstimateUSDCents_NegativeTokensZeroOut(t *testing.T) {
	// Defensive: a misbehaving provider that reports a negative count must
	// not subtract from the running USD total. Negatives are clamped to 0.
	if got := estimateUSDCents(-100, -50, 30, 60); got != 0 {
		t.Fatalf("estimateUSDCents(neg, neg, _, _) = %d, want 0", got)
	}
}

func TestParseNonNegativeInt64Env_UnsetReturnsZero(t *testing.T) {
	t.Setenv("UTA_PRICING_TEST_KEY", "")
	if got := parseNonNegativeInt64Env("UTA_PRICING_TEST_KEY"); got != 0 {
		t.Fatalf("unset env should yield 0, got %d", got)
	}
}

func TestParseNonNegativeInt64Env_GarbageReturnsZero(t *testing.T) {
	// A typo in the env var must not silently produce a charge — fall
	// back to 0 (USD accounting disabled) rather than crashing the run
	// or applying an undefined rate.
	t.Setenv("UTA_PRICING_TEST_KEY", "not-a-number")
	if got := parseNonNegativeInt64Env("UTA_PRICING_TEST_KEY"); got != 0 {
		t.Fatalf("garbage env value should yield 0, got %d", got)
	}
}

func TestParseNonNegativeInt64Env_NegativeReturnsZero(t *testing.T) {
	t.Setenv("UTA_PRICING_TEST_KEY", "-5")
	if got := parseNonNegativeInt64Env("UTA_PRICING_TEST_KEY"); got != 0 {
		t.Fatalf("negative env value should yield 0, got %d", got)
	}
}

func TestParseNonNegativeInt64Env_PositiveParses(t *testing.T) {
	t.Setenv("UTA_PRICING_TEST_KEY", "42")
	if got := parseNonNegativeInt64Env("UTA_PRICING_TEST_KEY"); got != 42 {
		t.Fatalf("env value 42 should parse, got %d", got)
	}
}

func TestGeminiPriceFromEnv_ReadsBothVars(t *testing.T) {
	t.Setenv("UTA_GEMINI_INPUT_CENTS_PER_KTOK", "15")
	t.Setenv("UTA_GEMINI_OUTPUT_CENTS_PER_KTOK", "75")
	in, out := geminiPriceFromEnv()
	if in != 15 || out != 75 {
		t.Fatalf("geminiPriceFromEnv() = (%d, %d), want (15, 75)", in, out)
	}
}

func TestGeminiPriceFromEnv_DefaultsToZero(t *testing.T) {
	// Without operator opt-in, both rates must be zero so the budget's
	// USD dimension remains inert for gemini calls.
	t.Setenv("UTA_GEMINI_INPUT_CENTS_PER_KTOK", "")
	t.Setenv("UTA_GEMINI_OUTPUT_CENTS_PER_KTOK", "")
	in, out := geminiPriceFromEnv()
	if in != 0 || out != 0 {
		t.Fatalf("unset rates should be zero, got (%d, %d)", in, out)
	}
}
