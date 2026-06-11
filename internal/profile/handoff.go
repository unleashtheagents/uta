package profile

import (
	"errors"
	"fmt"
	"path"
	"strings"
)

// Handoff describes a chained mode invocation that should fire after a
// MissionProfile's normal run completes. The orchestrator evaluates each
// Handoff in declaration order and dispatches the first one whose Condition
// matches; non-matching handoffs are silently skipped so a profile can
// declare several alternative follow-ups in one block.
//
// Field semantics:
//
//   - TargetMode:     the name of the MissionProfile to run next. Must
//     resolve via the normal profile.LoadAll override chain;
//     a missing target is a non-fatal warning at chain time.
//   - Condition:      structured gate evaluated against the prior run's
//     side effects (e.g. files changed by the worker). An
//     empty Condition always matches.
//   - PromptTemplate: the goal handed to the chained run. Supports a tiny
//     {{prior_session_id}} substitution so the next mode
//     can reference the upstream session in its own prompt.
type Handoff struct {
	TargetMode     string           `yaml:"target_mode"`
	Condition      HandoffCondition `yaml:"condition,omitempty"`
	PromptTemplate string           `yaml:"prompt"`
}

// HandoffCondition gates a Handoff against the prior run's effects.
//
// Both fields are zero-value-friendly: when empty / zero, that clause is
// considered satisfied. A Handoff with an entirely zero Condition always
// fires.
//
//   - FilesChangedMin: minimum count of files the prior run must have
//     changed (typically observed via `git diff
//     --name-only`) for the handoff to fire.
//   - ContainsAny:     glob patterns matched against the basename of each
//     changed file. At least one changed file must match
//     at least one pattern. Patterns use filepath.Match
//     syntax (e.g. "*.sol", "*.go").
type HandoffCondition struct {
	FilesChangedMin int      `yaml:"files_changed_min,omitempty"`
	ContainsAny     []string `yaml:"contains_any,omitempty"`
}

// Matches reports whether the condition is satisfied by the given set of
// changed file paths. An empty Condition trivially matches.
func (c HandoffCondition) Matches(changedFiles []string) bool {
	if c.FilesChangedMin > 0 && len(changedFiles) < c.FilesChangedMin {
		return false
	}
	if len(c.ContainsAny) == 0 {
		return true
	}
	for _, f := range changedFiles {
		base := path.Base(f)
		for _, pat := range c.ContainsAny {
			if ok, _ := path.Match(pat, base); ok {
				return true
			}
			// Also try matching against the full path so callers can
			// write patterns like "contracts/*.sol".
			if ok, _ := path.Match(pat, f); ok {
				return true
			}
		}
	}
	return false
}

// Validate checks the shape of a Handoff. Called by MissionProfile.Validate
// for each entry in OnComplete.
func (h *Handoff) Validate() error {
	if strings.TrimSpace(h.TargetMode) == "" {
		return errors.New("target_mode is required")
	}
	if strings.ContainsAny(h.TargetMode, "/\\") {
		return fmt.Errorf("target_mode %q must not contain path separators", h.TargetMode)
	}
	if h.Condition.FilesChangedMin < 0 {
		return fmt.Errorf("condition.files_changed_min must be >= 0 (got %d)", h.Condition.FilesChangedMin)
	}
	for i, pat := range h.Condition.ContainsAny {
		if strings.TrimSpace(pat) == "" {
			return fmt.Errorf("condition.contains_any[%d]: pattern must be non-empty", i)
		}
		if _, err := path.Match(pat, ""); err != nil {
			return fmt.Errorf("condition.contains_any[%d]: invalid pattern %q: %w", i, pat, err)
		}
	}
	return nil
}

// RenderPrompt substitutes {{prior_session_id}} in PromptTemplate with the
// supplied session id. Other text is returned verbatim. An empty template
// falls back to a stock "follow up on session <id>" sentence so a handoff
// without an explicit prompt still produces a sensible goal.
func (h *Handoff) RenderPrompt(priorSessionID string) string {
	tmpl := h.PromptTemplate
	if strings.TrimSpace(tmpl) == "" {
		tmpl = "Follow up on the work done in session {{prior_session_id}}."
	}
	return strings.ReplaceAll(tmpl, "{{prior_session_id}}", priorSessionID)
}
