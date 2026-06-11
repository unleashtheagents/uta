package engine

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Aderyn is Cyfrin's static analyzer for Solidity. Its `--output -` flag emits
// a single JSON report to stdout shaped roughly like:
//
//	{
//	  "high_issues":    {"issues": [{"title": "...", "description": "...", "instances": [{"contract_path": "...", "line_no": N}]}]},
//	  "low_issues":     {"issues": [...]},
//	  "nc_issues":      {"issues": [...]}
//	}
//
// Older versions used "results" instead of "issues" and a slightly flatter
// shape; the parser below accepts both. Each emitted detector becomes one
// Finding per instance, with severity derived from the bucket name.
//
// Adapter naming matches the slither adapter so the registry in tools.go can
// look it up by ID.

// parseAderyn decodes the JSON report aderyn writes to stdout when invoked
// with `aderyn --output -`. Falls back to silently returning nil findings when
// the payload is missing — aderyn exits 0 even when it has nothing to report
// for some configurations.
func parseAderyn(toolID, stdout, _ string) ([]Finding, error) {
	payload := extractJSONObject(stdout)
	if payload == "" {
		return nil, nil
	}
	var root aderynReport
	if err := json.Unmarshal([]byte(payload), &root); err != nil {
		return nil, fmt.Errorf("aderyn json: %w", err)
	}
	out := make([]Finding, 0)
	out = appendAderynBucket(out, toolID, "high", root.HighIssues.Issues)
	out = appendAderynBucket(out, toolID, "medium", root.MediumIssues.Issues)
	out = appendAderynBucket(out, toolID, "low", root.LowIssues.Issues)
	out = appendAderynBucket(out, toolID, "info", root.NCIssues.Issues)
	return out, nil
}

// aderynReport mirrors the bucketed shape aderyn currently emits. The
// `*_issues` keys are stable across the versions we've encountered; their
// inner `issues` array is what we iterate over.
type aderynReport struct {
	HighIssues   aderynBucket `json:"high_issues"`
	MediumIssues aderynBucket `json:"medium_issues"`
	LowIssues    aderynBucket `json:"low_issues"`
	NCIssues     aderynBucket `json:"nc_issues"`
}

type aderynBucket struct {
	Issues []aderynIssue `json:"issues"`
}

type aderynIssue struct {
	Title       string           `json:"title"`
	Description string           `json:"description"`
	Detector    string           `json:"detector_name"`
	Instances   []aderynInstance `json:"instances"`
}

type aderynInstance struct {
	ContractPath string `json:"contract_path"`
	Line         int    `json:"line_no"`
	// Older aderyn versions emit `src_char` or `src_line`; we honor both.
	SrcLine int `json:"src_line"`
}

func (i aderynInstance) line() int {
	if i.Line > 0 {
		return i.Line
	}
	return i.SrcLine
}

func appendAderynBucket(out []Finding, toolID, severity string, issues []aderynIssue) []Finding {
	sev := NormalizeSeverity(severity)
	for ix, is := range issues {
		base := Finding{
			Severity: sev,
			Title:    firstNonEmptyN(is.Title, is.Detector),
			Body:     strings.TrimSpace(is.Description),
		}
		detector := firstNonEmptyN(is.Detector, is.Title)
		if len(is.Instances) == 0 {
			base.ID = fmt.Sprintf("%s-%s-%d", toolID, slugify(detector), ix)
			out = append(out, base)
			continue
		}
		for jx, inst := range is.Instances {
			f := base
			f.ID = fmt.Sprintf("%s-%s-%d-%d", toolID, slugify(detector), ix, jx)
			f.File = inst.ContractPath
			if l := inst.line(); l > 0 {
				f.Line = l
			}
			out = append(out, f)
		}
	}
	return out
}

// firstNonEmptyN returns the first value whose trimmed form is non-empty.
// Named with the N suffix so it doesn't collide with the supervisor's
// two-arg firstNonEmpty helper.
func firstNonEmptyN(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// slugify produces a stable identifier fragment from a detector name. Only
// used to compose Finding.ID values; the result is not parsed back.
func slugify(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	if s == "" {
		return "issue"
	}
	var b strings.Builder
	b.Grow(len(s))
	prevDash := false
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			prevDash = false
		default:
			if !prevDash && b.Len() > 0 {
				b.WriteByte('-')
				prevDash = true
			}
		}
	}
	out := strings.TrimRight(b.String(), "-")
	if out == "" {
		return "issue"
	}
	return out
}
