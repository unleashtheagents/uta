// Package profile defines MissionProfiles — named bundles of personas,
// tool allow/deny lists, env vars, and policies that turn uta from
// one-shape-fits-all into modal. Profiles are discovered as YAML files
// under <project>/.uta/profiles/ and ~/.uta/profiles/; project-local
// definitions override global ones. A built-in "default" profile is
// always available so a fresh install has at least one mode.
//
// This package only parses, validates, and resolves the override chain.
// Wiring profiles into RunRequest / supervisor.go is the responsibility
// of a later iteration.
package profile

import (
	"errors"
	"fmt"
	"strings"
)

// MissionProfile is the on-disk shape of a YAML file under
// ~/.uta/profiles/<name>.yaml or <project>/.uta/profiles/<name>.yaml.
//
// Field semantics:
//
//   - Personas:     IDs of personas (from ~/.uta/personas/) the profile
//     wants attached to its workers. The actual wiring
//     happens in a later iteration.
//   - AllowedTools: tool-pattern strings. Treated as a pre-approve
//     allow-list in iter 2 and as a hard gate in iter 7.
//   - DeniedTools:  tool-pattern strings. DeniedTools wins over
//     AllowedTools on overlap (enforced by the future gate).
//   - Env:          KEY=VALUE pairs merged into the provider invocation
//     environment.
//   - Policies:     HITL severity threshold + token budgets. Surfaced
//     here for shape; enforcement lands later.
type MissionProfile struct {
	Name         string             `yaml:"name"`
	Description  string             `yaml:"description,omitempty"`
	Personas     []string           `yaml:"personas,omitempty"`
	AllowedTools []string           `yaml:"allowed_tools,omitempty"`
	DeniedTools  []string           `yaml:"denied_tools,omitempty"`
	Env          map[string]string  `yaml:"env,omitempty"`
	Policies     Policies           `yaml:"policies,omitempty"`
	MCPServers   []ProfileMCPServer `yaml:"mcp_servers,omitempty"`
	// Tools lists external analysis tool IDs (e.g. "slither", "aderyn")
	// the profile wants attached to its audits. These are passed straight
	// through to the audit engine's tool resolver — built-in adapter
	// names get default invocations; anything else is treated as a raw
	// shell command (see the CLI's --tool flag for the full grammar).
	Tools []string `yaml:"tools,omitempty"`

	// OnComplete declares chained mode invocations the orchestrator should
	// evaluate after the profile's normal run completes successfully. The
	// first Handoff whose Condition matches fires; the rest are skipped.
	// See Handoff for the per-entry semantics.
	OnComplete []Handoff `yaml:"on_complete,omitempty"`

	// MaxHandoffDepth caps how many chained mode invocations a single
	// `uta run` will follow before stopping. 0 (the zero value) means
	// "use the orchestrator default" (DefaultMaxHandoffDepth). Negative
	// values are rejected by Validate. The cap is a safety belt — the
	// trajectory event stream already carries enough info for Sentinel
	// to spot a tighter loop, but a hard ceiling guarantees the chain
	// can never run away.
	MaxHandoffDepth int `yaml:"max_handoff_depth,omitempty"`

	// Memory controls the profile's interaction with the cross-mode
	// institutional-memory store. Zero value disables both writes and
	// injection; see Memory's field comments for the read-side defaults.
	Memory MemoryConfig `yaml:"memory,omitempty"`

	// RetrospectiveEvery is the cadence (in completed sessions for this
	// mode) at which the orchestrator generates a LESSONS_LEARNED-style
	// retrospective markdown into
	// <project>/.uta/context/retrospectives/<mode>-<YYYYMMDD>.md.
	// 0 (the zero value) disables the feature entirely.
	RetrospectiveEvery int `yaml:"retrospective_every,omitempty"`

	// RetrospectivePrompt is the per-mode template used to generate the
	// retrospective. The string supports two placeholders:
	//   {{mode}}     — the profile name
	//   {{sessions}} — a deterministic plain-text summary of the last N
	//                  sessions (one bullet per session, with subtask
	//                  titles/statuses).
	// Empty string falls back to a built-in generic prompt.
	RetrospectivePrompt string `yaml:"retrospective_prompt,omitempty"`

	// ShadowDriftTokens overrides the shadow package's DefaultDriftTokens
	// when this profile is the target of a `uta shadow run`. 0 means
	// "use the package default"; the shadow CLI's --drift-tokens flag
	// (when non-zero) still takes precedence over this profile setting.
	ShadowDriftTokens int `yaml:"shadow_drift_tokens,omitempty"`

	// Source is populated by the loader. "embedded" for the built-in
	// default; absolute path for YAML-loaded profiles. Not user-settable.
	Source string `yaml:"-"`
}

// MemoryConfig is the per-profile institutional-memory opt-in. Consolidate
// triggers a post-run write of a session_outcome fact (deterministic, no
// LLM summarization in v1). TopK caps the number of relevant prior facts
// injected into the planner prompt — 0 means "use engine default" (3),
// negative disables injection entirely.
type MemoryConfig struct {
	Consolidate bool `yaml:"consolidate,omitempty"`
	TopK        int  `yaml:"top_k,omitempty"`
}

// Policies expresses guardrails that the supervisor enforces. HITLSeverity
// is the minimum severity attached to HITL pause events; "" disables the
// gate. HITLTriggers is a list of tool-pattern strings (same grammar as
// AllowedTools / DeniedTools — e.g. "Bash(* push *)") that trigger a
// human-approval pause when matched on a tool_call event. HITLTokenThreshold
// is a percent-of-TokenBudget threshold (e.g. 80) at which the gate fires
// once during a run; 0 disables it. TokenBudget is the hard cap on total
// tokens for the session; 0 means uncapped. PerCallBudget is the
// per-provider-call cap on tokens; 0 means uncapped. DollarBudgetCents is
// the hard cap on cumulative API spend for the session, expressed in U.S.
// cents (1 = $0.01); 0 means uncapped.
type Policies struct {
	HITLSeverity       string   `yaml:"hitl_severity,omitempty"`
	HITLTriggers       []string `yaml:"hitl_triggers,omitempty"`
	HITLTokenThreshold int      `yaml:"hitl_token_threshold,omitempty"`
	TokenBudget        int      `yaml:"token_budget,omitempty"`
	PerCallBudget      int      `yaml:"per_call_budget,omitempty"`
	DollarBudgetCents  int      `yaml:"dollar_budget_cents,omitempty"`
}

// DefaultProfileName is the name of the built-in profile. A user-written
// profile with this name overrides the embedded one.
const DefaultProfileName = "default"

// DefaultMaxHandoffDepth is the depth limit applied when a profile leaves
// MaxHandoffDepth at zero. Chosen to fit the typical dev → audit → ops →
// audit chain (four hops) and no more — anything longer is almost always
// a misconfiguration that warrants operator review.
const DefaultMaxHandoffDepth = 4

// MaxHandoffDepthHardLimit is the upper bound a profile may request. It
// exists to keep a typo in YAML (e.g. 4000 instead of 4) from spawning a
// pathologically long chain. Profiles requesting more are rejected at
// validation time.
const MaxHandoffDepthHardLimit = 32

// EmbeddedSource is the value placed in MissionProfile.Source for the
// built-in default profile.
const EmbeddedSource = "embedded"

// validHITLSeverities is the closed set of accepted severity values for
// Policies.HITLSeverity. An empty string disables the gate entirely.
var validHITLSeverities = map[string]bool{
	"":         true,
	"low":      true,
	"medium":   true,
	"high":     true,
	"critical": true,
}

// Validate checks the shape of a parsed MissionProfile. It is called by
// the loader after YAML parsing.
func (p *MissionProfile) Validate() error {
	name := strings.TrimSpace(p.Name)
	if name == "" {
		return errors.New("name is required")
	}
	if name != p.Name {
		return errors.New("name must not contain leading or trailing whitespace")
	}
	if strings.ContainsAny(name, "/\\") {
		return fmt.Errorf("name %q must not contain path separators", name)
	}
	if !validHITLSeverities[p.Policies.HITLSeverity] {
		return fmt.Errorf("policies.hitl_severity %q is not one of: low, medium, high, critical (or empty to disable)", p.Policies.HITLSeverity)
	}
	if p.Policies.TokenBudget < 0 {
		return fmt.Errorf("policies.token_budget must be >= 0 (got %d)", p.Policies.TokenBudget)
	}
	if p.Policies.PerCallBudget < 0 {
		return fmt.Errorf("policies.per_call_budget must be >= 0 (got %d)", p.Policies.PerCallBudget)
	}
	if p.Policies.DollarBudgetCents < 0 {
		return fmt.Errorf("policies.dollar_budget_cents must be >= 0 (got %d)", p.Policies.DollarBudgetCents)
	}
	if p.Policies.HITLTokenThreshold < 0 || p.Policies.HITLTokenThreshold > 100 {
		return fmt.Errorf("policies.hitl_token_threshold must be in [0,100] (got %d)", p.Policies.HITLTokenThreshold)
	}
	if p.RetrospectiveEvery < 0 {
		return fmt.Errorf("retrospective_every must be >= 0 (got %d)", p.RetrospectiveEvery)
	}
	if p.ShadowDriftTokens < 0 {
		return fmt.Errorf("shadow_drift_tokens must be >= 0 (got %d)", p.ShadowDriftTokens)
	}
	if p.MaxHandoffDepth < 0 {
		return fmt.Errorf("max_handoff_depth must be >= 0 (got %d)", p.MaxHandoffDepth)
	}
	if p.MaxHandoffDepth > MaxHandoffDepthHardLimit {
		return fmt.Errorf("max_handoff_depth must be <= %d (got %d); a longer chain is almost always a misconfiguration", MaxHandoffDepthHardLimit, p.MaxHandoffDepth)
	}
	for k := range p.Env {
		if strings.TrimSpace(k) == "" {
			return errors.New("env: variable name must be non-empty")
		}
		if strings.ContainsRune(k, '=') {
			return fmt.Errorf("env: variable name %q must not contain '='", k)
		}
	}
	seen := make(map[string]bool, len(p.MCPServers))
	for i := range p.MCPServers {
		m := &p.MCPServers[i]
		if err := m.Validate(); err != nil {
			return fmt.Errorf("mcp_servers[%d]: %w", i, err)
		}
		if seen[m.Name] {
			return fmt.Errorf("mcp_servers: duplicate server name %q", m.Name)
		}
		seen[m.Name] = true
	}
	for i := range p.OnComplete {
		h := &p.OnComplete[i]
		if err := h.Validate(); err != nil {
			return fmt.Errorf("on_complete[%d]: %w", i, err)
		}
	}
	return nil
}

// defaultProfileYAML is the embedded source of the built-in "default"
// profile. Kept in sync with examples/profiles/default.yaml by hand; the
// example file carries explanatory comments while this constant is the
// minimal canonical form the loader instantiates when no on-disk profile
// shadows it.
const defaultProfileYAML = `name: default
description: |
  Baseline mode — preserves uta's pre-MissionProfile behavior. No persona
  injection, no tool restrictions, no token budgets, HITL disabled.
personas: []
allowed_tools: []
denied_tools: []
env: {}
policies:
  hitl_severity: ""
  token_budget: 0
  per_call_budget: 0
`

// EmbeddedDefault returns a freshly-parsed copy of the built-in "default"
// profile. Returns a new instance each call so callers can mutate without
// stomping on the embedded source.
func EmbeddedDefault() *MissionProfile {
	p, err := parseProfile([]byte(defaultProfileYAML), EmbeddedSource)
	if err != nil {
		// The embedded YAML is a Go const we control; a parse failure
		// here is a programmer bug, not a user-facing condition.
		panic(fmt.Sprintf("embedded default profile is invalid: %v", err))
	}
	return p
}
