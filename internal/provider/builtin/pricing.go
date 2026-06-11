package builtin

import (
	"os"
	"strconv"
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
