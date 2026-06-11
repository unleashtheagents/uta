// Package redact scrubs secret-shaped strings from text before it
// leaves the local machine. Trajectories record raw provider output —
// tool results can contain environment dumps, key files, and tokens the
// operator never intended to share. Every outward-facing surface
// (uta exportdb, the MCP read tools) runs its payloads through this
// package by default.
//
// Design notes:
//   - Pattern-based, not entropy-based. High-entropy detection has too
//     many false positives on base64 blob refs and git SHAs, which uta
//     output is full of. Each pattern below targets a documented,
//     recognizable credential shape.
//   - Replacement preserves enough context to debug ("[REDACTED:aws-key]")
//     without preserving any of the secret material.
//   - The scrubber is pure: no I/O, no config files. Callers that need
//     to bypass it (local backup, debugging) pass an explicit opt-out
//     at the CLI layer (--no-redact), keeping the default path safe.
package redact

import (
	"fmt"
	"regexp"
)

// rule pairs a credential shape with its replacement label.
type rule struct {
	name string
	re   *regexp.Regexp
}

// rules covers the credential shapes most likely to appear in tool
// output. Sources: vendor docs + gitleaks/trufflehog default configs.
// Order matters only for overlapping matches (earlier wins via the
// sequential pass); keep the more specific shapes first.
var rules = []rule{
	// Private key blocks — PEM-armored. Multiline; match the whole block.
	{"private-key", regexp.MustCompile(`(?s)-----BEGIN [A-Z ]*PRIVATE KEY-----.*?-----END [A-Z ]*PRIVATE KEY-----`)},

	// AWS access key id (AKIA/ASIA + 16 uppercase alnum) and secret pairs.
	{"aws-key", regexp.MustCompile(`\b(?:AKIA|ASIA)[0-9A-Z]{16}\b`)},

	// GitHub tokens: classic (ghp_), fine-grained (github_pat_), app
	// (ghs_/ghu_), OAuth (gho_), refresh (ghr_).
	{"github-token", regexp.MustCompile(`\b(?:ghp|ghs|ghu|gho|ghr)_[A-Za-z0-9]{36,}\b|\bgithub_pat_[A-Za-z0-9_]{22,}\b`)},

	// Google API key.
	{"google-api-key", regexp.MustCompile(`\bAIza[0-9A-Za-z\-_]{35}\b`)},

	// Anthropic API key.
	{"anthropic-key", regexp.MustCompile(`\bsk-ant-[A-Za-z0-9\-_]{20,}\b`)},

	// OpenAI / generic sk- keys (sk- + 20+ chars; after sk-ant so the
	// more specific label wins for Anthropic keys).
	{"sk-key", regexp.MustCompile(`\bsk-[A-Za-z0-9\-_]{20,}\b`)},

	// Slack tokens (bot/user/app-level).
	{"slack-token", regexp.MustCompile(`\bxox[baprs]-[A-Za-z0-9\-]{10,}\b`)},

	// Stripe live/test secret keys.
	{"stripe-key", regexp.MustCompile(`\b[sr]k_(?:live|test)_[A-Za-z0-9]{20,}\b`)},

	// JWTs — three dot-separated base64url segments, header starts eyJ.
	{"jwt", regexp.MustCompile(`\beyJ[A-Za-z0-9\-_]{10,}\.[A-Za-z0-9\-_]{10,}\.[A-Za-z0-9\-_]{10,}\b`)},

	// Authorization headers: Bearer/Basic + token material.
	{"auth-header", regexp.MustCompile(`(?i)\b(?:authorization\s*[:=]\s*)?(?:bearer|basic)\s+[A-Za-z0-9\-._~+/=]{16,}`)},

	// Env-style assignments whose key name suggests secret material.
	// Captures KEY=value / KEY: value / "key": "value" forms. The value
	// part stops at whitespace / quote / comma so surrounding JSON or
	// shell text survives.
	{"env-secret", regexp.MustCompile(`(?i)\b([A-Z0-9_]*(?:SECRET|TOKEN|PASSWORD|PASSWD|API_KEY|APIKEY|ACCESS_KEY|PRIVATE_KEY|CREDENTIALS)[A-Z0-9_]*)(["']?\s*[:=]\s*["']?)([^\s"',;]{8,})`)},
}

// envSecretRuleName must match the rule above that uses capture groups —
// it is replaced with key+separator preserved, value redacted.
const envSecretRuleName = "env-secret"

// Result reports what String did, so callers can surface a summary
// ("redacted 3 secrets") without diffing the text themselves.
type Result struct {
	// Counts maps rule name → number of replacements applied.
	Counts map[string]int
}

// Total returns the total number of replacements across all rules.
func (r Result) Total() int {
	n := 0
	for _, c := range r.Counts {
		n += c
	}
	return n
}

// String scrubs every recognized secret shape from s and reports what
// was replaced. Safe on empty input (returns "" and an empty Result).
func String(s string) (string, Result) {
	res := Result{Counts: map[string]int{}}
	if s == "" {
		return s, res
	}
	for _, r := range rules {
		if r.name == envSecretRuleName {
			s = r.re.ReplaceAllStringFunc(s, func(m string) string {
				sub := r.re.FindStringSubmatch(m)
				if len(sub) != 4 {
					return m
				}
				res.Counts[r.name]++
				return sub[1] + sub[2] + "[REDACTED:" + r.name + "]"
			})
			continue
		}
		s = r.re.ReplaceAllStringFunc(s, func(string) string {
			res.Counts[r.name]++
			return fmt.Sprintf("[REDACTED:%s]", r.name)
		})
	}
	return s, res
}

// Bytes is the []byte convenience wrapper around String.
func Bytes(b []byte) ([]byte, Result) {
	out, res := String(string(b))
	return []byte(out), res
}

// Merge folds other's counts into r (in place on the map). Convenient
// for callers scrubbing many fields who want one summary.
func (r *Result) Merge(other Result) {
	if r.Counts == nil {
		r.Counts = map[string]int{}
	}
	for k, v := range other.Counts {
		r.Counts[k] += v
	}
}
