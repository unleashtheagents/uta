package engine

import (
	"path/filepath"
	"strings"

	"github.com/unleashtheagents/uta/internal/version"
)

// SARIF 2.1.0 subset, just enough to make our FindingsReport consumable by
// GitHub Code Scanning, Sonarqube, IDEs, etc. We deliberately ship a tiny
// hand-rolled subset rather than a generated full-schema struct — the spec
// has hundreds of optional fields we don't use.
//
// Spec: https://docs.oasis-open.org/sarif/sarif/v2.1.0/

type SARIF struct {
	Schema  string     `json:"$schema"`
	Version string     `json:"version"`
	Runs    []SARIFRun `json:"runs"`
}

type SARIFRun struct {
	Tool    SARIFTool     `json:"tool"`
	Results []SARIFResult `json:"results"`
}

type SARIFTool struct {
	Driver SARIFDriver `json:"driver"`
}

type SARIFDriver struct {
	Name           string `json:"name"`
	Version        string `json:"version"`
	InformationURI string `json:"informationUri,omitempty"`
}

type SARIFResult struct {
	RuleID    string          `json:"ruleId"`
	Level     string          `json:"level"` // error | warning | note | none
	Message   SARIFMessage    `json:"message"`
	Locations []SARIFLocation `json:"locations,omitempty"`
	// Properties carry the original critic ID and uta-specific severity
	// so downstream tools that understand "more than SARIF level" can
	// still consume them.
	Properties map[string]any `json:"properties,omitempty"`
}

type SARIFMessage struct {
	Text string `json:"text"`
}

type SARIFLocation struct {
	PhysicalLocation SARIFPhysicalLocation `json:"physicalLocation"`
}

type SARIFPhysicalLocation struct {
	ArtifactLocation SARIFArtifactLocation `json:"artifactLocation"`
	Region           *SARIFRegion          `json:"region,omitempty"`
}

type SARIFArtifactLocation struct {
	URI string `json:"uri"`
}

type SARIFRegion struct {
	StartLine int `json:"startLine,omitempty"`
}

// ToSARIF converts a FindingsReport into a SARIF 2.1.0 document.
// auditRoot is the absolute path the audit was rooted at; file paths in
// findings are emitted relative to that root when possible.
func ToSARIF(r *FindingsReport, auditRoot string) SARIF {
	if r == nil {
		return SARIF{
			Schema:  "https://raw.githubusercontent.com/oasis-tcs/sarif-spec/main/Schemata/sarif-schema-2.1.0.json",
			Version: "2.1.0",
			Runs:    []SARIFRun{{Tool: SARIFTool{Driver: utaDriver()}, Results: []SARIFResult{}}},
		}
	}
	results := make([]SARIFResult, 0, len(r.Findings))
	for _, f := range r.Findings {
		body := f.Body
		if body == "" {
			body = f.Title
		}
		props := map[string]any{
			"critic":   f.Critic,
			"severity": string(f.Severity),
			"title":    f.Title,
		}
		if f.SubtaskID != "" {
			props["subtask_id"] = f.SubtaskID
		}
		res := SARIFResult{
			RuleID:     f.ID,
			Level:      severityToSARIFLevel(f.Severity),
			Message:    SARIFMessage{Text: body},
			Properties: props,
		}
		if f.File != "" {
			loc := SARIFLocation{
				PhysicalLocation: SARIFPhysicalLocation{
					ArtifactLocation: SARIFArtifactLocation{URI: sarifRelPath(f.File, auditRoot)},
				},
			}
			if f.Line > 0 {
				loc.PhysicalLocation.Region = &SARIFRegion{StartLine: f.Line}
			}
			res.Locations = append(res.Locations, loc)
		}
		results = append(results, res)
	}
	return SARIF{
		Schema:  "https://raw.githubusercontent.com/oasis-tcs/sarif-spec/main/Schemata/sarif-schema-2.1.0.json",
		Version: "2.1.0",
		Runs: []SARIFRun{{
			Tool:    SARIFTool{Driver: utaDriver()},
			Results: results,
		}},
	}
}

func utaDriver() SARIFDriver {
	return SARIFDriver{
		Name:           "uta",
		Version:        version.Version,
		InformationURI: "https://unleashtheagents.ai",
	}
}

// sarifRelPath converts a finding's file path to one relative to auditRoot
// when possible. GitHub Code Scanning and most SARIF consumers expect
// repo-relative paths; absolute paths leak the runner's filesystem layout
// and break cross-environment portability. Falls back to the original path
// if no rebase is feasible (empty root, non-absolute path, different
// volume on Windows, or escapes the root).
func sarifRelPath(file, auditRoot string) string {
	if auditRoot == "" || !filepath.IsAbs(file) {
		return filepath.ToSlash(file)
	}
	rel, err := filepath.Rel(auditRoot, file)
	if err != nil {
		return filepath.ToSlash(file)
	}
	if strings.HasPrefix(rel, "..") {
		return filepath.ToSlash(file)
	}
	return filepath.ToSlash(rel)
}

func severityToSARIFLevel(s Severity) string {
	switch s {
	case SevHigh:
		return "error"
	case SevMedium:
		return "warning"
	case SevLow, SevInfo:
		return "note"
	}
	return "none"
}
