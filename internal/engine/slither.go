package engine

import (
	"encoding/json"
	"fmt"
	"strings"
)

// parseSlither decodes Slither's `--json -` output. The schema:
//
//	{"success":bool,"error":...,"results":{"detectors":[{...}]}}
//
// Each detector has: check (rule id), impact (severity), confidence,
// description, elements[].source_mapping.{filename_relative,lines[]}.
//
// Kept in its own file (mirroring aderyn.go) so the adapter set is
// browsable as one-tool-per-file. parseSlither is referenced from
// tools.go's adapterRegistry; moving the function here keeps the
// registry intact.
func parseSlither(toolID, stdout, _ string) ([]Finding, error) {
	payload := extractJSONObject(stdout)
	if payload == "" {
		return nil, nil
	}
	var root struct {
		Success bool `json:"success"`
		Error   any  `json:"error"`
		Results struct {
			Detectors []struct {
				Check       string `json:"check"`
				Impact      string `json:"impact"`
				Confidence  string `json:"confidence"`
				Description string `json:"description"`
				Elements    []struct {
					Type          string `json:"type"`
					Name          string `json:"name"`
					SourceMapping struct {
						FilenameRelative string `json:"filename_relative"`
						Lines            []int  `json:"lines"`
					} `json:"source_mapping"`
				} `json:"elements"`
			} `json:"detectors"`
		} `json:"results"`
	}
	if err := json.Unmarshal([]byte(payload), &root); err != nil {
		return nil, fmt.Errorf("slither json: %w", err)
	}
	out := make([]Finding, 0, len(root.Results.Detectors))
	for i, d := range root.Results.Detectors {
		base := Finding{
			Severity: NormalizeSeverity(d.Impact),
			Title:    fmt.Sprintf("%s (%s)", d.Check, d.Confidence),
			Body:     strings.TrimSpace(d.Description),
		}
		if len(d.Elements) == 0 {
			base.ID = fmt.Sprintf("%s-%d", d.Check, i)
			out = append(out, base)
			continue
		}
		for j, el := range d.Elements {
			f := base
			f.ID = fmt.Sprintf("%s-%d-%d", d.Check, i, j)
			f.File = el.SourceMapping.FilenameRelative
			if len(el.SourceMapping.Lines) > 0 {
				f.Line = el.SourceMapping.Lines[0]
			}
			out = append(out, f)
		}
	}
	return out, nil
}
