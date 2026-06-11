package engine

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

// Severity is the ordered audit-finding severity tag. Lower-cased on parse.
type Severity string

const (
	SevHigh    Severity = "high"
	SevMedium  Severity = "medium"
	SevLow     Severity = "low"
	SevInfo    Severity = "info"
	SevUnknown Severity = "unknown"
)

// Rank returns a comparable integer where higher = more severe.
func (s Severity) Rank() int {
	switch s {
	case SevHigh:
		return 4
	case SevMedium:
		return 3
	case SevLow:
		return 2
	case SevInfo:
		return 1
	}
	return 0
}

// NormalizeSeverity coerces common spellings to a canonical Severity.
func NormalizeSeverity(s string) Severity {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "high", "critical", "crit", "severe", "h":
		return SevHigh
	case "medium", "med", "moderate", "m":
		return SevMedium
	case "low", "minor", "l":
		return SevLow
	case "info", "informational", "note", "nit", "low-info", "i":
		return SevInfo
	}
	return SevUnknown
}

// Finding is one issue surfaced by an auditor critic (or tool gate).
type Finding struct {
	Critic    string   `json:"critic"`               // which critic produced this
	SubtaskID string   `json:"subtask_id,omitempty"` // subtask that produced this — links back to trajectory events and raw provider output
	ID        string   `json:"id"`                   // critic-local identifier; used for dedup within a critic
	Severity  Severity `json:"severity"`
	Title     string   `json:"title"`
	Body      string   `json:"body,omitempty"`
	File      string   `json:"file,omitempty"`
	Line      int      `json:"line,omitempty"`
	Snippet   string   `json:"snippet,omitempty"`
}

// FindingsReport is the per-iteration aggregate written to the project's
// context store as findings.<N>.json (and copied to findings.json for the
// latest iteration).
type FindingsReport struct {
	Iteration  int            `json:"iteration"`
	ProducedAt time.Time      `json:"produced_at"`
	Critics    []string       `json:"critics"`
	Findings   []Finding      `json:"findings"`
	Stats      map[string]int `json:"stats"` // counts keyed by severity
}

// ParseFindings extracts a list of findings from a critic's free-form text
// output. Permissive: prefers an object literal that actually starts with
// `{"findings":` (extracted via string-aware brace matching, so braces in the
// LLM's prose preamble don't break parsing); otherwise falls back to first `{`
// to last `}` (or first `[` to last `]` for a bare array).
//
// Returns an error wrapping ErrFindingsUnparseable when nothing can be
// recovered. Empty findings is valid (returns nil, nil).
func ParseFindings(text string) ([]Finding, error) {
	if candidate, ok := findJSONObjectByKey(text, "findings"); ok {
		var parsed struct {
			Findings []Finding `json:"findings"`
		}
		if err := json.Unmarshal([]byte(candidate), &parsed); err != nil {
			return nil, fmt.Errorf("unmarshal object: %w: %v", ErrFindingsUnparseable, err)
		}
		return normalize(parsed.Findings), nil
	}
	start := strings.Index(text, "{")
	startA := strings.Index(text, "[")
	// Prefer whichever opens first.
	if startA >= 0 && (start < 0 || startA < start) {
		end := strings.LastIndex(text, "]")
		if end <= startA {
			return nil, fmt.Errorf("no closing bracket: %w", ErrFindingsUnparseable)
		}
		var raw []Finding
		if err := json.Unmarshal([]byte(text[startA:end+1]), &raw); err != nil {
			return nil, fmt.Errorf("unmarshal bare array: %w: %v", ErrFindingsUnparseable, err)
		}
		return normalize(raw), nil
	}
	if start < 0 {
		return nil, fmt.Errorf("no JSON shape in critic output: %w", ErrFindingsUnparseable)
	}
	end := strings.LastIndex(text, "}")
	if end <= start {
		return nil, fmt.Errorf("no closing brace: %w", ErrFindingsUnparseable)
	}
	var obj struct {
		Findings []Finding `json:"findings"`
	}
	if err := json.Unmarshal([]byte(text[start:end+1]), &obj); err != nil {
		return nil, fmt.Errorf("unmarshal object: %w: %v", ErrFindingsUnparseable, err)
	}
	return normalize(obj.Findings), nil
}

func normalize(in []Finding) []Finding {
	out := make([]Finding, 0, len(in))
	for _, f := range in {
		f.Severity = NormalizeSeverity(string(f.Severity))
		if strings.TrimSpace(f.Title) == "" && strings.TrimSpace(f.Body) == "" {
			continue // empty finding — drop
		}
		out = append(out, f)
	}
	return out
}

// ErrFindingsUnparseable signals the critic returned something we couldn't
// turn into a list of findings.
var ErrFindingsUnparseable = errors.New("critic output unparseable")

// Aggregate merges per-critic findings into a single sorted report. Findings
// are sorted by severity descending then by critic+id.
//
// Dedup runs in two passes to reduce noise the reviser would otherwise face:
//  1. within a critic, findings sharing the same ID collapse to one (highest
//     severity wins; ties keep the first occurrence).
//  2. across critics, findings pinned to the same (file, line) and normalized
//     title collapse to one — different critics flagging the same spot in the
//     same way is duplicate noise. Higher severity wins; ties go to the
//     alphabetically-first critic.
//
// Findings without enough information to form a stable dedup key (no ID, no
// file/line) are left untouched.
func Aggregate(iteration int, perCritic map[string][]Finding) FindingsReport {
	rep := FindingsReport{
		Iteration:  iteration,
		ProducedAt: time.Now().UTC(),
		Stats:      map[string]int{},
	}
	for critic := range perCritic {
		rep.Critics = append(rep.Critics, critic)
	}
	sort.Strings(rep.Critics)

	// Pass 1: dedup within each critic (deterministic order by sorted critic).
	for _, critic := range rep.Critics {
		seen := map[string]int{} // key -> index into rep.Findings
		for _, f := range perCritic[critic] {
			f.Critic = critic
			key := withinCriticKey(f)
			if key == "" {
				rep.Findings = append(rep.Findings, f)
				continue
			}
			if idx, ok := seen[key]; ok {
				if f.Severity.Rank() > rep.Findings[idx].Severity.Rank() {
					rep.Findings[idx] = f
				}
				continue
			}
			seen[key] = len(rep.Findings)
			rep.Findings = append(rep.Findings, f)
		}
	}

	// Pass 2: dedup across critics on (file, line, title).
	if len(rep.Findings) > 1 {
		seen := map[string]int{}
		collapsed := rep.Findings[:0:0]
		for _, f := range rep.Findings {
			key := crossCriticKey(f)
			if key == "" {
				collapsed = append(collapsed, f)
				continue
			}
			if idx, ok := seen[key]; ok {
				cur := collapsed[idx]
				if f.Severity.Rank() > cur.Severity.Rank() ||
					(f.Severity.Rank() == cur.Severity.Rank() && f.Critic < cur.Critic) {
					collapsed[idx] = f
				}
				continue
			}
			seen[key] = len(collapsed)
			collapsed = append(collapsed, f)
		}
		rep.Findings = collapsed
	}

	sort.SliceStable(rep.Findings, func(i, j int) bool {
		ri, rj := rep.Findings[i].Severity.Rank(), rep.Findings[j].Severity.Rank()
		if ri != rj {
			return ri > rj
		}
		if rep.Findings[i].Critic != rep.Findings[j].Critic {
			return rep.Findings[i].Critic < rep.Findings[j].Critic
		}
		return rep.Findings[i].ID < rep.Findings[j].ID
	})
	for _, f := range rep.Findings {
		rep.Stats[string(f.Severity)]++
	}
	return rep
}

// withinCriticKey is the dedup key for two findings emitted by the same critic.
// Prefers the critic-local ID; falls back to (file, line, normalized title).
func withinCriticKey(f Finding) string {
	if id := strings.TrimSpace(f.ID); id != "" {
		return "id:" + strings.ToLower(id)
	}
	title := strings.TrimSpace(strings.ToLower(f.Title))
	if title == "" || f.File == "" {
		return ""
	}
	return fmt.Sprintf("t:%s|f:%s|l:%d", title, f.File, f.Line)
}

// crossCriticKey is the dedup key for two findings emitted by different
// critics. Requires a real (file, line) location and a non-empty title — we
// don't want to collapse two distinct "missing-error-wrap" findings just
// because their bodies happen to start the same way.
func crossCriticKey(f Finding) string {
	if f.File == "" || f.Line <= 0 {
		return ""
	}
	title := strings.TrimSpace(strings.ToLower(f.Title))
	if title == "" {
		return ""
	}
	return fmt.Sprintf("%s|%d|%s", f.File, f.Line, title)
}

// HighestSeverity returns the most severe finding's tier in the report, or
// SevUnknown when the report has zero findings.
func (r *FindingsReport) HighestSeverity() Severity {
	max := SevUnknown
	for _, f := range r.Findings {
		if f.Severity.Rank() > max.Rank() {
			max = f.Severity
		}
	}
	return max
}
