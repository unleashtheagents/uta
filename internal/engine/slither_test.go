package engine

import (
	"strings"
	"testing"
)

// slitherSampleJSON mirrors the shape of `slither --json -` output for the
// VulnerableToken.sol fixture under tests/fixtures/contracts/. Two detectors
// fire — a HIGH reentrancy + a LOW missing zero-address check — and a third
// detector emits no elements to exercise the no-elements branch.
const slitherSampleJSON = `{
  "success": true,
  "error": null,
  "results": {
    "detectors": [
      {
        "check": "reentrancy-eth",
        "impact": "High",
        "confidence": "High",
        "description": "Reentrancy in withdraw (VulnerableToken.sol#49-54)",
        "elements": [
          {
            "type": "function",
            "name": "withdraw",
            "source_mapping": {
              "filename_relative": "contracts/VulnerableToken.sol",
              "lines": [49, 50, 51, 52, 53, 54]
            }
          }
        ]
      },
      {
        "check": "missing-zero-check",
        "impact": "Low",
        "confidence": "Medium",
        "description": "transfer.to lacks a zero-address check",
        "elements": [
          {
            "type": "parameter",
            "name": "to",
            "source_mapping": {
              "filename_relative": "contracts/VulnerableToken.sol",
              "lines": [41]
            }
          },
          {
            "type": "parameter",
            "name": "newAdmin",
            "source_mapping": {
              "filename_relative": "contracts/VulnerableToken.sol",
              "lines": [57]
            }
          }
        ]
      },
      {
        "check": "solc-version",
        "impact": "Informational",
        "confidence": "High",
        "description": "Floating pragma is not recommended",
        "elements": []
      }
    ]
  }
}`

func TestParseSlither_DetectorsAndElements(t *testing.T) {
	got, err := parseSlither("slither", slitherSampleJSON, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// 1 (reentrancy element) + 2 (missing-zero-check elements) + 1 (solc-version, no elements) = 4
	if len(got) != 4 {
		t.Fatalf("expected 4 findings, got %d: %+v", len(got), got)
	}

	var highCount, lowCount, infoCount int
	var sawReentrancy, sawSecondZeroCheckInstance bool
	for _, f := range got {
		switch f.Severity {
		case SevHigh:
			highCount++
		case SevLow:
			lowCount++
		case SevInfo:
			infoCount++
		}
		if strings.HasPrefix(f.Title, "reentrancy-eth") {
			sawReentrancy = true
			if f.File != "contracts/VulnerableToken.sol" {
				t.Errorf("reentrancy finding file = %q, want contracts/VulnerableToken.sol", f.File)
			}
			if f.Line != 49 {
				t.Errorf("reentrancy finding line = %d, want 49 (first in source_mapping.lines)", f.Line)
			}
		}
		if strings.HasPrefix(f.Title, "missing-zero-check") && f.Line == 57 {
			sawSecondZeroCheckInstance = true
		}
	}
	if highCount != 1 {
		t.Errorf("expected 1 HIGH finding, got %d", highCount)
	}
	if lowCount != 2 {
		t.Errorf("expected 2 LOW findings (one per zero-check element), got %d", lowCount)
	}
	if infoCount != 1 {
		t.Errorf("expected 1 INFO finding for the elements-less detector, got %d", infoCount)
	}
	if !sawReentrancy {
		t.Errorf("expected reentrancy-eth detector to surface in findings: %+v", got)
	}
	if !sawSecondZeroCheckInstance {
		t.Errorf("expected the second missing-zero-check element (line 57) to surface: %+v", got)
	}
}

func TestParseSlither_IDsAreStableAndUnique(t *testing.T) {
	got, err := parseSlither("slither", slitherSampleJSON, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	seen := map[string]bool{}
	for _, f := range got {
		if f.ID == "" {
			t.Errorf("finding has empty ID: %+v", f)
		}
		if seen[f.ID] {
			t.Errorf("duplicate finding ID %q across %d findings", f.ID, len(got))
		}
		seen[f.ID] = true
	}
}

func TestParseSlither_EmptyStdout(t *testing.T) {
	got, err := parseSlither("slither", "", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("expected zero findings on empty input, got %+v", got)
	}
}

func TestParseSlither_NoJSONFallsThrough(t *testing.T) {
	got, err := parseSlither("slither", "slither: scanning...\n", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("expected zero findings on prose-only stdout, got %+v", got)
	}
}

func TestParseSlither_MalformedJSON(t *testing.T) {
	_, err := parseSlither("slither", `{"results": "not an object"}`, "")
	if err == nil {
		t.Fatal("expected JSON unmarshal error for results being a string")
	}
	if !strings.Contains(err.Error(), "slither json") {
		t.Errorf("error should be wrapped with 'slither json' context, got %v", err)
	}
}

func TestParseSlither_PrependedBanner(t *testing.T) {
	// slither prints log lines + a Python deprecation warning before the
	// JSON payload in some configurations. The parser must extract the
	// balanced object regardless of the banner.
	input := "INFO:Detectors:\nrunning detectors...\n" + slitherSampleJSON
	got, err := parseSlither("slither", input, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) == 0 {
		t.Fatalf("expected findings extracted past the banner, got none")
	}
}

func TestResolveToolSpec_SlitherDefaults(t *testing.T) {
	got := ResolveToolSpec(ToolSpec{ID: "slither"})
	if got.Cmd != "slither" {
		t.Errorf("expected default cmd 'slither', got %q", got.Cmd)
	}
	if got.Adapter != "slither" {
		t.Errorf("expected default adapter 'slither', got %q", got.Adapter)
	}
	if len(got.Args) < 3 || got.Args[0] != "." || got.Args[1] != "--json" || got.Args[2] != "-" {
		t.Errorf("unexpected default args: %v", got.Args)
	}
	if !got.ContinueOnFailure {
		t.Errorf("slither exits 255 on findings; ContinueOnFailure should be true (got false)")
	}
}

func TestKnownBuiltinTools_IncludesSlither(t *testing.T) {
	tools := KnownBuiltinTools()
	for _, name := range tools {
		if name == "slither" {
			return
		}
	}
	t.Fatalf("slither missing from KnownBuiltinTools: %v", tools)
}
