package builtin

import (
	"os"
	"strconv"
	"strings"
)

// geminiPriceFromEnv reads the operator-configured per-1k-token cent rates
// for gemini from environment variables. Returns 0/0 when the operator has
// not opted in, which suppresses USD accounting for gemini calls (the same
// posture as a hardcoded zero, but now explicitly controllable).
//
// Variables (set by the operator in their shell, profile env, or .envrc):
//
//	UTA_GEMINI_INPUT_CENTS_PER_KTOK   — U.S. cents per 1,000 input tokens
//	UTA_GEMINI_OUTPUT_CENTS_PER_KTOK  — U.S. cents per 1,000 output tokens
//
// The gemini CLI does not stream cost the way claude's does, so without
// these the dollar dimension of Budget.MaxUSDCents never trips for gemini.
// We deliberately read process env (not opts.Env / profile env) so the same
// rate applies to every gemini call in the process, matching how an
// operator would set "what does my gemini model cost me right now."
func geminiPriceFromEnv() (inputCentsPerKtok, outputCentsPerKtok int64) {
	return parseNonNegativeInt64Env("UTA_GEMINI_INPUT_CENTS_PER_KTOK"),
		parseNonNegativeInt64Env("UTA_GEMINI_OUTPUT_CENTS_PER_KTOK")
}

// agyPriceFromEnv reads the operator-configured per-1k-token cent rates for
// Google Antigravity CLI (agy), falling back to the gemini rates when the
// agy-specific variables are unset.
func agyPriceFromEnv() (inputCentsPerKtok, outputCentsPerKtok int64) {
	in := parseNonNegativeInt64Env("UTA_AGY_INPUT_CENTS_PER_KTOK")
	out := parseNonNegativeInt64Env("UTA_AGY_OUTPUT_CENTS_PER_KTOK")
	if in == 0 && out == 0 {
		return geminiPriceFromEnv()
	}
	return in, out
}

// nebiusPriceFromEnv reads the operator-configured per-1k-token cent rates
// for Nebius Token Factory (NVIDIA Nemotron models). When set they override
// the built-in per-model list prices below.
func nebiusPriceFromEnv() (inputCentsPerKtok, outputCentsPerKtok int64) {
	return parseNonNegativeInt64Env("UTA_NEBIUS_INPUT_CENTS_PER_KTOK"),
		parseNonNegativeInt64Env("UTA_NEBIUS_OUTPUT_CENTS_PER_KTOK")
}

// NebiusListPrice is a Nebius Token Factory pay-as-you-go list price for one
// Nemotron model, in U.S. cents per one million tokens.
type NebiusListPrice struct {
	InputCentsPerMtok  int64
	OutputCentsPerMtok int64
	// Assumed marks a tier whose price was not published at the time of
	// writing and is carried over from the closest sibling tier.
	Assumed bool
}

// nebiusListPrices holds the built-in per-model list prices (USD per 1M
// tokens, as cents): Ultra $1.00/$3.00, Super $0.30/$0.90, Nano $0.06/$0.24,
// Lightning $0.06/$0.24. Super-VL had no published price and is assumed to
// match Super. Snapshot taken 2026-10-27 from public listings of Nebius Token
// Factory pricing; override with UTA_NEBIUS_{INPUT,OUTPUT}_CENTS_PER_KTOK or
// disable with UTA_NEBIUS_PRICING=off. These exist so that a dollar budget
// (`budget ... $1` in steer, --budget-usd in uta run) actually enforces on
// Nebius calls instead of silently accounting zero.
var nebiusListPrices = map[string]NebiusListPrice{
	NebiusModelUltra:     {InputCentsPerMtok: 100, OutputCentsPerMtok: 300},
	NebiusModelSuper:     {InputCentsPerMtok: 30, OutputCentsPerMtok: 90},
	NebiusModelNano:      {InputCentsPerMtok: 6, OutputCentsPerMtok: 24},
	NebiusModelLightning: {InputCentsPerMtok: 6, OutputCentsPerMtok: 24},
	NebiusModelSuperVL:   {InputCentsPerMtok: 30, OutputCentsPerMtok: 90, Assumed: true},
}

// NebiusListPriceFor returns the built-in list price for a resolved Nebius
// model ID. ok is false for models outside the Nemotron catalog.
func NebiusListPriceFor(model string) (NebiusListPrice, bool) {
	lp, ok := nebiusListPrices[ResolveNebiusModel(model)]
	return lp, ok
}

func nebiusPricingDisabled(env []string) bool {
	v := strings.ToLower(resolveEnvKey(env, "UTA_NEBIUS_PRICING"))
	return v == "off" || v == "0" || v == "false"
}

// NebiusEstimateUSD returns the precise (unrounded) U.S. dollar cost of a
// (tokensIn, tokensOut) pair on the given Nemotron model, using operator env
// rates when set and the built-in list prices otherwise. ok is false when no
// price applies (unknown model, or pricing disabled).
func NebiusEstimateUSD(env []string, model string, tokensIn, tokensOut int64) (usd float64, ok bool) {
	if nebiusPricingDisabled(env) {
		return 0, false
	}
	if inK, outK := nebiusPriceFromEnv(); inK > 0 || outK > 0 {
		return (float64(tokensIn)*float64(inK) + float64(tokensOut)*float64(outK)) / 1000 / 100, true
	}
	lp, ok := NebiusListPriceFor(model)
	if !ok {
		return 0, false
	}
	return (float64(tokensIn)*float64(lp.InputCentsPerMtok) + float64(tokensOut)*float64(lp.OutputCentsPerMtok)) / 1e6 / 100, true
}

// nebiusEstimateUSDCents is the budget-facing integer form: total cost in
// cents, rounded up once over both dimensions so a dollar cap is a
// pessimistic upper bound but tiny calls are not each charged a full cent per
// dimension.
func nebiusEstimateUSDCents(env []string, model string, tokensIn, tokensOut int64) int64 {
	usd, ok := NebiusEstimateUSD(env, model, tokensIn, tokensOut)
	if !ok || usd <= 0 {
		return 0
	}
	cents := usd * 100
	whole := int64(cents)
	if float64(whole) < cents {
		whole++
	}
	return whole
}

func parseNonNegativeInt64Env(key string) int64 {
	v := os.Getenv(key)
	if v == "" {
		return 0
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n < 0 {
		return 0
	}
	return n
}

// estimateUSDCents multiplies a (tokensIn, tokensOut) usage pair by per-1k
// cent rates and returns the total. Rounding is ceiling so the budget gate
// treats the estimate as a pessimistic upper bound — same posture the
// gemini provider uses for token estimation. Returns 0 when both rates are
// 0 (operator did not configure pricing).
func estimateUSDCents(tokensIn, tokensOut, inputCentsPerKtok, outputCentsPerKtok int64) int64 {
	if inputCentsPerKtok <= 0 && outputCentsPerKtok <= 0 {
		return 0
	}
	return ceilDivide1k(tokensIn, inputCentsPerKtok) + ceilDivide1k(tokensOut, outputCentsPerKtok)
}

// ceilDivide1k returns ceil(tokens * rate / 1000) for non-negative inputs.
// Negative or zero rates contribute zero so a caller may pass a zero rate
// to disable one dimension without affecting the other.
func ceilDivide1k(tokens, rate int64) int64 {
	if tokens <= 0 || rate <= 0 {
		return 0
	}
	return (tokens*rate + 999) / 1000
}
