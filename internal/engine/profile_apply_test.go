package engine

import (
	"reflect"
	"testing"

	"github.com/unleashtheagents/uta/internal/profile"
)

func TestApplyProfile_FiltersDisallowedPreApprove(t *testing.T) {
	p := &profile.MissionProfile{
		Name: "dev",
		AllowedTools: []string{
			"Read", "Edit", "Write",
			"Bash(go *)", "Bash(git status)", "Bash(git diff)",
		},
	}
	req := &RunRequest{
		PreApproveTools: []string{"Read", "Bash(curl *)", "Edit"},
	}
	ApplyProfile(req, p)

	want := []string{"Read", "Edit"}
	if !reflect.DeepEqual(req.PreApproveTools, want) {
		t.Errorf("PreApproveTools = %v, want %v (Bash(curl *) must be filtered)", req.PreApproveTools, want)
	}
}

func TestApplyProfile_EmptyAllowedToolsLeavesUserListAlone(t *testing.T) {
	p := &profile.MissionProfile{Name: "default"}
	req := &RunRequest{PreApproveTools: []string{"Read", "Edit"}}
	ApplyProfile(req, p)
	want := []string{"Read", "Edit"}
	if !reflect.DeepEqual(req.PreApproveTools, want) {
		t.Errorf("empty AllowedTools should pass through unchanged, got %v", req.PreApproveTools)
	}
}

func TestApplyProfile_MergesEnv(t *testing.T) {
	p := &profile.MissionProfile{
		Name: "dev",
		Env:  map[string]string{"FOO": "bar", "BAZ": "qux"},
	}
	req := &RunRequest{Env: []string{"EXISTING=1"}}
	ApplyProfile(req, p)

	want := []string{"EXISTING=1", "BAZ=qux", "FOO=bar"}
	if !reflect.DeepEqual(req.Env, want) {
		t.Errorf("Env = %v, want %v", req.Env, want)
	}
}

func TestApplyProfile_NilSafe(t *testing.T) {
	ApplyProfile(nil, nil)
	ApplyProfile(&RunRequest{}, nil)
	ApplyProfile(nil, &profile.MissionProfile{Name: "x"})
}

// fullPolicyProfile returns a MissionProfile with every policy axis set
// so the Apply* helpers can be tested for coverage of all fields. Used
// by the ToResume / ToReflector tests below.
func fullPolicyProfile() *profile.MissionProfile {
	return &profile.MissionProfile{
		Name:         "audit",
		Env:          map[string]string{"AUDITOR": "trail-of-bits"},
		AllowedTools: []string{"Read", "Bash(slither *)"},
		DeniedTools:  []string{"Bash(rm *)"},
		Policies: profile.Policies{
			TokenBudget:        100_000,
			PerCallBudget:      10_000,
			DollarBudgetCents:  500,
			HITLTriggers:       []string{"Bash(* push *)"},
			HITLTokenThreshold: 80,
			HITLSeverity:       "high",
		},
		Memory: profile.MemoryConfig{Consolidate: true, TopK: 5},
	}
}

func TestApplyProfileToResume_PopulatesEveryField(t *testing.T) {
	req := &ResumeRequest{PreApproveTools: []string{"Read", "Bash(rm *)", "Bash(slither x)"}}
	ApplyProfileToResume(req, fullPolicyProfile())

	if req.ModeName != "audit" {
		t.Errorf("ModeName = %q, want audit", req.ModeName)
	}
	if !reflect.DeepEqual(req.AllowedTools, []string{"Read", "Bash(slither *)"}) {
		t.Errorf("AllowedTools = %v", req.AllowedTools)
	}
	if !reflect.DeepEqual(req.DeniedTools, []string{"Bash(rm *)"}) {
		t.Errorf("DeniedTools = %v", req.DeniedTools)
	}
	// PreApproveTools must intersect with AllowedTools AND subtract DeniedTools.
	// "Bash(slither x)" is NOT in AllowedTools (which contains "Bash(slither *)"),
	// so the intersection drops it — the pre-approve list uses exact-string
	// equality, pattern matching happens at gate time only.
	if !reflect.DeepEqual(req.PreApproveTools, []string{"Read"}) {
		t.Errorf("PreApproveTools = %v, want [Read]", req.PreApproveTools)
	}
	if req.MaxTokens != 100_000 || req.PerCallMaxTokens != 10_000 || req.MaxUSDCents != 500 {
		t.Errorf("budget caps = (%d, %d, %d)", req.MaxTokens, req.PerCallMaxTokens, req.MaxUSDCents)
	}
	if !reflect.DeepEqual(req.HITLTriggers, []string{"Bash(* push *)"}) {
		t.Errorf("HITLTriggers = %v", req.HITLTriggers)
	}
	if req.HITLTokenThreshold != 80 || req.HITLSeverity != "high" {
		t.Errorf("HITL fields = (%d, %q)", req.HITLTokenThreshold, req.HITLSeverity)
	}
}

func TestApplyProfileToResume_NilSafe(t *testing.T) {
	ApplyProfileToResume(nil, nil)
	ApplyProfileToResume(&ResumeRequest{}, nil)
	ApplyProfileToResume(nil, &profile.MissionProfile{Name: "x"})
}

func TestApplyProfileToReflector_PopulatesEveryField(t *testing.T) {
	req := &ReflectorRequest{}
	ApplyProfileToReflector(req, fullPolicyProfile())

	if req.ModeName != "audit" {
		t.Errorf("ModeName = %q", req.ModeName)
	}
	if req.MaxTokens != 100_000 || req.MaxUSDCents != 500 {
		t.Errorf("budget caps not copied")
	}
	if !req.MemoryConsolidate {
		t.Errorf("MemoryConsolidate should be true")
	}
	if !reflect.DeepEqual(req.HITLTriggers, []string{"Bash(* push *)"}) {
		t.Errorf("HITLTriggers = %v", req.HITLTriggers)
	}
}

func TestApplyProfileToReflector_NilSafe(t *testing.T) {
	ApplyProfileToReflector(nil, nil)
	ApplyProfileToReflector(&ReflectorRequest{}, nil)
}

func TestIntersectAllowedTools_EmptyUser(t *testing.T) {
	got := IntersectAllowedTools(nil, []string{"Read"})
	if len(got) != 0 {
		t.Errorf("empty user list should produce empty intersection, got %v", got)
	}
}

func TestApplyProfile_PopulatesGateLists(t *testing.T) {
	p := &profile.MissionProfile{
		Name:         "dev",
		AllowedTools: []string{"Read", "Edit"},
		DeniedTools:  []string{"Bash(curl *)"},
	}
	req := &RunRequest{}
	ApplyProfile(req, p)

	if !reflect.DeepEqual(req.AllowedTools, []string{"Read", "Edit"}) {
		t.Errorf("AllowedTools on req: got %v want [Read Edit]", req.AllowedTools)
	}
	if !reflect.DeepEqual(req.DeniedTools, []string{"Bash(curl *)"}) {
		t.Errorf("DeniedTools on req: got %v want [Bash(curl *)]", req.DeniedTools)
	}
}

func TestApplyProfile_CopiesHITLFields(t *testing.T) {
	p := &profile.MissionProfile{
		Name: "ops",
		Policies: profile.Policies{
			HITLSeverity:       "high",
			HITLTriggers:       []string{"Bash(* push *)", "mcp__gmail__send_*"},
			HITLTokenThreshold: 75,
		},
	}
	req := &RunRequest{}
	ApplyProfile(req, p)

	if req.HITLSeverity != "high" {
		t.Errorf("HITLSeverity = %q want high", req.HITLSeverity)
	}
	if !reflect.DeepEqual(req.HITLTriggers, []string{"Bash(* push *)", "mcp__gmail__send_*"}) {
		t.Errorf("HITLTriggers = %v", req.HITLTriggers)
	}
	if req.HITLTokenThreshold != 75 {
		t.Errorf("HITLTokenThreshold = %d want 75", req.HITLTokenThreshold)
	}
}

func TestApplyProfile_DeniedToolStrippedFromPreApprove(t *testing.T) {
	// User passes --pre-approve including a tool the profile denies.
	// Deny must win even if AllowedTools is empty (no positive restriction
	// at the intersect step) — otherwise the user could smuggle a denied
	// tool past the gate via the pre-approve list.
	p := &profile.MissionProfile{
		Name:        "dev",
		DeniedTools: []string{"Bash(curl *)"},
	}
	req := &RunRequest{
		PreApproveTools: []string{"Read", "Bash(curl *)", "Edit"},
	}
	ApplyProfile(req, p)

	want := []string{"Read", "Edit"}
	if !reflect.DeepEqual(req.PreApproveTools, want) {
		t.Errorf("PreApproveTools = %v, want %v (denied entry must be removed)", req.PreApproveTools, want)
	}
}

func TestApplyProfile_DeniedTrumpsAllowedOnCollision(t *testing.T) {
	// A pattern listed in BOTH allowed and denied is denied. Pre-approve
	// must reflect that even if the user passed the pattern explicitly.
	p := &profile.MissionProfile{
		Name:         "weird",
		AllowedTools: []string{"Read", "Bash(curl *)"},
		DeniedTools:  []string{"Bash(curl *)"},
	}
	req := &RunRequest{
		PreApproveTools: []string{"Read", "Bash(curl *)"},
	}
	ApplyProfile(req, p)

	want := []string{"Read"}
	if !reflect.DeepEqual(req.PreApproveTools, want) {
		t.Errorf("PreApproveTools = %v, want %v (deny wins over allow)", req.PreApproveTools, want)
	}
}

func TestApplyProfile_DoesNotMutateProfile(t *testing.T) {
	// ApplyProfile must be pure with respect to its profile argument so
	// callers can reuse the same MissionProfile across multiple runs (a
	// chained handoff, for example). Pin that contract by snapshotting
	// every slice/map field and comparing after the call.
	p := &profile.MissionProfile{
		Name:         "dev",
		AllowedTools: []string{"Read", "Edit"},
		DeniedTools:  []string{"Bash(curl *)"},
		Env:          map[string]string{"FOO": "bar"},
		Policies: profile.Policies{
			HITLSeverity:       "high",
			HITLTriggers:       []string{"Bash(* push *)"},
			HITLTokenThreshold: 75,
			TokenBudget:        100000,
		},
	}

	wantAllow := append([]string(nil), p.AllowedTools...)
	wantDeny := append([]string(nil), p.DeniedTools...)
	wantTriggers := append([]string(nil), p.Policies.HITLTriggers...)
	wantEnv := map[string]string{}
	for k, v := range p.Env {
		wantEnv[k] = v
	}

	req := &RunRequest{PreApproveTools: []string{"Read", "Bash(curl *)"}}
	ApplyProfile(req, p)

	// Mutate the request's copied slices — must not leak back to p.
	if len(req.AllowedTools) > 0 {
		req.AllowedTools[0] = "STOMPED"
	}
	if len(req.DeniedTools) > 0 {
		req.DeniedTools[0] = "STOMPED"
	}
	if len(req.HITLTriggers) > 0 {
		req.HITLTriggers[0] = "STOMPED"
	}

	if !reflect.DeepEqual(p.AllowedTools, wantAllow) {
		t.Errorf("p.AllowedTools mutated: got %v, want %v", p.AllowedTools, wantAllow)
	}
	if !reflect.DeepEqual(p.DeniedTools, wantDeny) {
		t.Errorf("p.DeniedTools mutated: got %v, want %v", p.DeniedTools, wantDeny)
	}
	if !reflect.DeepEqual(p.Policies.HITLTriggers, wantTriggers) {
		t.Errorf("p.Policies.HITLTriggers mutated: got %v, want %v", p.Policies.HITLTriggers, wantTriggers)
	}
	if !reflect.DeepEqual(p.Env, wantEnv) {
		t.Errorf("p.Env mutated: got %v, want %v", p.Env, wantEnv)
	}
}

func TestSubtractDeniedTools_NoOpOnEmptyDeny(t *testing.T) {
	in := []string{"Read", "Edit"}
	got := SubtractDeniedTools(in, nil)
	if !reflect.DeepEqual(got, in) {
		t.Errorf("empty deny should pass through unchanged, got %v", got)
	}
}

func TestApplyProfile_CopiesBudgetCaps(t *testing.T) {
	p := &profile.MissionProfile{
		Name: "frugal",
		Policies: profile.Policies{
			TokenBudget:       100000,
			PerCallBudget:     8000,
			DollarBudgetCents: 250,
		},
	}
	req := &RunRequest{}
	ApplyProfile(req, p)

	if req.MaxTokens != 100000 {
		t.Errorf("MaxTokens: got %d want 100000", req.MaxTokens)
	}
	if req.PerCallMaxTokens != 8000 {
		t.Errorf("PerCallMaxTokens: got %d want 8000", req.PerCallMaxTokens)
	}
	if req.MaxUSDCents != 250 {
		t.Errorf("MaxUSDCents: got %d want 250", req.MaxUSDCents)
	}
}
