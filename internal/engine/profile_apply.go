package engine

import (
	"sort"

	"github.com/unleashtheagents/uta/internal/profile"
)

// ApplyProfile transforms req in-place using the supplied MissionProfile.
// It merges p.Env into req.Env (appended last so duplicate keys win when
// expanded into os/exec env), restricts req.PreApproveTools to the
// intersection of the user-supplied list and p.AllowedTools, removes any
// exact-match DeniedTools entries from req.PreApproveTools (so a user's
// --pre-approve cannot smuggle a denied tool past the gate), and copies
// the AllowedTools / DeniedTools lists onto the request so the
// supervisor's capability gate can enforce them at the trajectory layer.
//
// Precedence on the pre-approve list:
//
//  1. AllowedTools (when non-empty) acts as a hard allow-list — entries
//     not in it are dropped from PreApproveTools.
//  2. DeniedTools removes any remaining exact-string matches from
//     PreApproveTools. Deny always wins on collision.
//
// The runtime capability gate (built from AllowedTools/DeniedTools)
// enforces the same precedence at every tool_call event, using pattern
// semantics (globs in argument positions). The pre-approve filter only
// does exact-string equality because pre-approve is a hint we forward
// verbatim to the provider; pattern-aware filtering happens at gate time.
// An empty AllowedTools means "no positive restriction" — the user's
// pre-approve list passes through unchanged except for explicit deny hits.
//
// ApplyProfile is pure with respect to p: it copies every slice/map it
// reads, so callers may safely reuse the same MissionProfile across runs.
//
// No-op when either argument is nil.
func ApplyProfile(req *RunRequest, p *profile.MissionProfile) {
	if req == nil || p == nil {
		return
	}
	req.Env = MergeProfileEnv(req.Env, p.Env)
	req.PreApproveTools = IntersectAllowedTools(req.PreApproveTools, p.AllowedTools)
	req.PreApproveTools = SubtractDeniedTools(req.PreApproveTools, p.DeniedTools)
	req.AllowedTools = append([]string(nil), p.AllowedTools...)
	req.DeniedTools = append([]string(nil), p.DeniedTools...)
	req.OnComplete = append([]profile.Handoff(nil), p.OnComplete...)
	req.MaxHandoffDepth = p.MaxHandoffDepth
	req.ModeName = p.Name
	req.MemoryConsolidate = p.Memory.Consolidate
	req.MemoryTopK = p.Memory.TopK
	// Resource-budget caps. Profile values flow through as int; the
	// supervisor stores them as int64 to match the Budget API and avoid
	// silent narrowing on 32-bit builds.
	req.MaxTokens = int64(p.Policies.TokenBudget)
	req.PerCallMaxTokens = int64(p.Policies.PerCallBudget)
	req.MaxUSDCents = int64(p.Policies.DollarBudgetCents)
	// HITL trigger config — patterns + budget-threshold percent + the
	// severity tag attached to the resulting trajectory events. The
	// approver itself (the human-talking-to-the-process side) is wired
	// by the CLI, not the profile.
	req.HITLTriggers = append([]string(nil), p.Policies.HITLTriggers...)
	req.HITLTokenThreshold = p.Policies.HITLTokenThreshold
	req.HITLSeverity = p.Policies.HITLSeverity
}

// ApplyProfileToReflector mirrors ApplyProfile for ReflectorRequest.
// Audit runs share the same orchestration policies as run / resume / improve.
//
// Capability-gate and budget enforcement at the Reflector supervisor path
// is deferred to a follow-up commit: the fields land here so call sites
// can be wired now and the engine-side change stays local.
func ApplyProfileToReflector(req *ReflectorRequest, p *profile.MissionProfile) {
	if req == nil || p == nil {
		return
	}
	req.Env = MergeProfileEnv(req.Env, p.Env)
	req.PreApproveTools = IntersectAllowedTools(req.PreApproveTools, p.AllowedTools)
	req.PreApproveTools = SubtractDeniedTools(req.PreApproveTools, p.DeniedTools)
	req.AllowedTools = append([]string(nil), p.AllowedTools...)
	req.DeniedTools = append([]string(nil), p.DeniedTools...)
	req.ModeName = p.Name
	req.MaxTokens = int64(p.Policies.TokenBudget)
	req.PerCallMaxTokens = int64(p.Policies.PerCallBudget)
	req.MaxUSDCents = int64(p.Policies.DollarBudgetCents)
	req.HITLTriggers = append([]string(nil), p.Policies.HITLTriggers...)
	req.HITLTokenThreshold = p.Policies.HITLTokenThreshold
	req.HITLSeverity = p.Policies.HITLSeverity
	req.MemoryConsolidate = p.Memory.Consolidate
}

// ApplyProfileToResume mirrors ApplyProfile for ResumeRequest. Resume turns
// use the same orchestration policies — capability gates, HITL triggers,
// budget caps — as run turns; the helper exists because ResumeRequest is a
// narrower struct (no planner / synthesis / handoff) and we don't want
// callers to remember which subset applies.
//
// Budget caps (MaxTokens / MaxUSDCents / PerCallMaxTokens) flow through but
// are not enforced by the supervisor's Resume path as of this commit — the
// fields are set so a future commit can wire the budget accumulator into
// Resume without changing this helper's signature.
func ApplyProfileToResume(req *ResumeRequest, p *profile.MissionProfile) {
	if req == nil || p == nil {
		return
	}
	req.Env = MergeProfileEnv(req.Env, p.Env)
	req.PreApproveTools = IntersectAllowedTools(req.PreApproveTools, p.AllowedTools)
	req.PreApproveTools = SubtractDeniedTools(req.PreApproveTools, p.DeniedTools)
	req.AllowedTools = append([]string(nil), p.AllowedTools...)
	req.DeniedTools = append([]string(nil), p.DeniedTools...)
	req.ModeName = p.Name
	req.MaxTokens = int64(p.Policies.TokenBudget)
	req.PerCallMaxTokens = int64(p.Policies.PerCallBudget)
	req.MaxUSDCents = int64(p.Policies.DollarBudgetCents)
	req.HITLTriggers = append([]string(nil), p.Policies.HITLTriggers...)
	req.HITLTokenThreshold = p.Policies.HITLTokenThreshold
	req.HITLSeverity = p.Policies.HITLSeverity
}

// MergeProfileEnv appends KEY=VALUE pairs from in to existing. Keys are
// emitted in sorted order so two runs with the same profile produce
// byte-identical env slices. Returns existing unchanged when in is empty.
func MergeProfileEnv(existing []string, in map[string]string) []string {
	if len(in) == 0 {
		return existing
	}
	keys := make([]string, 0, len(in))
	for k := range in {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]string, 0, len(existing)+len(in))
	out = append(out, existing...)
	for _, k := range keys {
		out = append(out, k+"="+in[k])
	}
	return out
}

// IntersectAllowedTools returns the elements of user that also appear in
// allowed. When allowed is empty, user is returned unchanged (an empty
// allow-list means the profile imposes no restriction at this layer).
// Order of user is preserved; duplicates are kept as-is.
func IntersectAllowedTools(user, allowed []string) []string {
	if len(allowed) == 0 {
		return user
	}
	allowSet := make(map[string]struct{}, len(allowed))
	for _, t := range allowed {
		allowSet[t] = struct{}{}
	}
	out := make([]string, 0, len(user))
	for _, t := range user {
		if _, ok := allowSet[t]; ok {
			out = append(out, t)
		}
	}
	return out
}

// SubtractDeniedTools removes from user every element whose raw string
// equals an entry in denied. Matching is exact-string only; the runtime
// capability gate handles pattern-aware enforcement. When denied is
// empty, user is returned unchanged. Order of user is preserved.
func SubtractDeniedTools(user, denied []string) []string {
	if len(denied) == 0 {
		return user
	}
	denySet := make(map[string]struct{}, len(denied))
	for _, t := range denied {
		denySet[t] = struct{}{}
	}
	out := make([]string, 0, len(user))
	for _, t := range user {
		if _, ok := denySet[t]; ok {
			continue
		}
		out = append(out, t)
	}
	return out
}
