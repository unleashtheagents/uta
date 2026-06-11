package engine

import (
	"strings"
	"testing"
)

const aderynSampleJSON = `{
  "high_issues": {
    "issues": [
      {
        "title": "Reentrancy in withdraw",
        "description": "External call before state mutation",
        "detector_name": "reentrancy-eth",
        "instances": [
          {"contract_path": "VulnerableToken.sol", "line_no": 50}
        ]
      }
    ]
  },
  "low_issues": {
    "issues": [
      {
        "title": "Missing zero address check",
        "description": "transfer() does not check recipient",
        "detector_name": "zero-address-check",
        "instances": [
          {"contract_path": "VulnerableToken.sol", "line_no": 41},
          {"contract_path": "VulnerableToken.sol", "line_no": 60}
        ]
      }
    ]
  },
  "nc_issues": {
    "issues": [
      {
        "title": "Use of solc default",
        "description": "Floating pragma is risky",
        "detector_name": "pragma-version",
        "instances": []
      }
    ]
  }
}`

func TestParseAderyn_BucketedIssues(t *testing.T) {
	got, err := parseAderyn("aderyn", aderynSampleJSON, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 4 {
		t.Fatalf("expected 4 findings (1 high + 2 low instances + 1 nc), got %d: %+v", len(got), got)
	}

	var highCount, lowCount, infoCount int
	var seenReentrancy bool
	for _, f := range got {
		switch f.Severity {
		case SevHigh:
			highCount++
		case SevLow:
			lowCount++
		case SevInfo:
			infoCount++
		}
		if strings.Contains(strings.ToLower(f.Title), "reentrancy") {
			seenReentrancy = true
			if f.File != "VulnerableToken.sol" {
				t.Errorf("expected reentrancy finding to be located in VulnerableToken.sol, got %q", f.File)
			}
			if f.Line != 50 {
				t.Errorf("expected reentrancy finding at line 50, got %d", f.Line)
			}
		}
	}
	if highCount != 1 {
		t.Errorf("expected 1 HIGH finding, got %d", highCount)
	}
	if lowCount != 2 {
		t.Errorf("expected 2 LOW findings (one per instance), got %d", lowCount)
	}
	if infoCount != 1 {
		t.Errorf("expected 1 INFO finding for nc bucket, got %d", infoCount)
	}
	if !seenReentrancy {
		t.Errorf("expected the reentrancy detector to surface in findings: %+v", got)
	}
}

func TestParseAderyn_EmptyStdout(t *testing.T) {
	got, err := parseAderyn("aderyn", "", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("expected zero findings on empty input, got %+v", got)
	}
}

func TestParseAderyn_NoJSONFallsThrough(t *testing.T) {
	got, err := parseAderyn("aderyn", "aderyn: scanning...\n", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("expected zero findings on prose-only stdout, got %+v", got)
	}
}

func TestParseAderyn_MalformedJSON(t *testing.T) {
	_, err := parseAderyn("aderyn", `{"high_issues": "not an object"}`, "")
	if err == nil {
		t.Fatal("expected JSON unmarshal error")
	}
}

func TestParseAderyn_PrependedBanner(t *testing.T) {
	// aderyn prints version banners + progress lines before the JSON
	// payload in some configurations. The parser must extract the
	// balanced object regardless.
	input := "aderyn 0.5.0\nscanning project root...\n" + aderynSampleJSON
	got, err := parseAderyn("aderyn", input, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) == 0 {
		t.Fatalf("expected findings extracted past the banner, got none")
	}
}

func TestResolveToolSpec_AderynDefaults(t *testing.T) {
	got := ResolveToolSpec(ToolSpec{ID: "aderyn"})
	if got.Cmd != "aderyn" {
		t.Errorf("expected default cmd 'aderyn', got %q", got.Cmd)
	}
	if got.Adapter != "aderyn" {
		t.Errorf("expected default adapter 'aderyn', got %q", got.Adapter)
	}
	if len(got.Args) < 3 || got.Args[0] != "." || got.Args[1] != "--output" || got.Args[2] != "-" {
		t.Errorf("unexpected default args: %v", got.Args)
	}
	if got.ContinueOnFailure {
		t.Errorf("aderyn exits 0 on findings; ContinueOnFailure should be false (got true)")
	}
}

func TestKnownBuiltinTools_IncludesAderyn(t *testing.T) {
	tools := KnownBuiltinTools()
	for _, t := range tools {
		if t == "aderyn" {
			return
		}
	}
	t.Fatalf("aderyn missing from KnownBuiltinTools: %v", tools)
}

func TestSlugify(t *testing.T) {
	cases := map[string]string{
		"reentrancy-eth":          "reentrancy-eth",
		"Reentrancy ETH":          "reentrancy-eth",
		"  zero address  check  ": "zero-address-check",
		"":                        "issue",
		"!!!":                     "issue",
		"foo--bar":                "foo-bar",
		"UPPER_case_name":         "upper-case-name",
		"trailing!":               "trailing",
	}
	for in, want := range cases {
		got := slugify(in)
		if got != want {
			t.Errorf("slugify(%q) = %q, want %q", in, got, want)
		}
	}
}
