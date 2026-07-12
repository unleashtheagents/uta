package cli

import "testing"

func TestDisabledProviderSet(t *testing.T) {
	cases := []struct {
		env  string
		want map[string]bool
	}{
		{"", map[string]bool{}},
		{"gemini", map[string]bool{"gemini": true}},
		{"Gemini, CLAUDE ", map[string]bool{"gemini": true, "claude": true}},
		{",,gemini,", map[string]bool{"gemini": true}},
	}
	for _, c := range cases {
		t.Setenv("UTA_DISABLED_PROVIDERS", c.env)
		got := disabledProviderSet()
		if len(got) != len(c.want) {
			t.Fatalf("env %q: got %v, want %v", c.env, got, c.want)
		}
		for k := range c.want {
			if !got[k] {
				t.Fatalf("env %q: missing %q in %v", c.env, k, got)
			}
		}
	}
}
