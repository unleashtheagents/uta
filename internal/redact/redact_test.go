package redact

import (
	"strings"
	"testing"
)

// Test fixtures are assembled by concatenation at runtime so no
// secret-shaped literal ever appears in this file (or in git history).
// Secret scanners — including GitHub push protection — pattern-match
// file contents; a verbatim fake token would trip them forever even
// though it's synthetic. The runtime-assembled strings still exercise
// the exact same regexes.
func fx(parts ...string) string { return strings.Join(parts, "") }

func TestString_RedactsKnownShapes(t *testing.T) {
	cases := []struct {
		name string
		in   string
		rule string
		leak string // substring that must NOT survive
	}{
		{"aws", "creds: " + fx("AKIA", "IOSFODNN7EXAMPLE") + " done", "aws-key", fx("AKIA", "IOSFODNN7EXAMPLE")},
		{"github-classic", "token " + fx("ghp_", "abcdefghijklmnopqrstuvwxyz0123456789"), "github-token", fx("ghp_", "abcdef")},
		{"github-fine", fx("github_pat_", "11ABCDEFG0123456789abc_xyz"), "github-token", fx("github_pat_", "11ABCDEFG")},
		{"google", "key=" + fx("AIza", "SyA-1234567890abcdefghijklmnopqrstu"), "google-api-key", fx("AIza", "SyA-")},
		{"anthropic", fx("sk-ant-", "api03-abcdefghijklmnopqrstuv"), "anthropic-key", fx("sk-ant-", "api03")},
		{"openai", fx("sk-", "abcdefghijklmnopqrstuvwxyz123456"), "sk-key", fx("sk-", "abcdefghijklmnop")},
		{"slack", fx("xoxb-", "123456789012-abcdefghijklmnop"), "slack-token", fx("xoxb-", "12345")},
		{"stripe", fx("sk_live_", "abcdefghijklmnopqrstuvwx"), "stripe-key", fx("sk_live_", "abcdef")},
		{"jwt", fx("eyJ", "hbGciOiJIUzI1NiJ9", ".", "eyJzdWIiOiIxMjM0NTY3ODkwIn0", ".", "dozjgNryP4J3jVmNHl0w5N_XgL0n3I9PlFUP0THsR8U"), "jwt", "dozjgNryP4J3jVmNHl0w5N"},
		{"bearer", "Authorization: " + fx("Bearer ", "abcdefghij0123456789xyz"), "auth-header", "abcdefghij0123456789"},
		{"env-upper", "export " + fx("MY_API_KEY", "=", "supersecretvalue123"), "env-secret", "supersecretvalue123"},
		{"env-json", fx(`"DATABASE_PASSWORD"`, `: `, `"hunter2hunter2"`), "env-secret", "hunter2hunter2"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, res := String(tc.in)
			if strings.Contains(out, tc.leak) {
				t.Errorf("leak survived: %q in %q", tc.leak, out)
			}
			if res.Counts[tc.rule] == 0 {
				t.Errorf("expected rule %q to fire, counts=%v, out=%q", tc.rule, res.Counts, out)
			}
			if !strings.Contains(out, "[REDACTED:") {
				t.Errorf("no redaction marker in output: %q", out)
			}
		})
	}
}

func TestString_PrivateKeyBlock(t *testing.T) {
	in := strings.Join([]string{
		"prefix",
		fx("-----BEGIN RSA ", "PRIVATE KEY-----"),
		"MIIEowIBAAKCAQEA0Z3VS5JJcds3xfn/ygWy",
		"F0Z3VS5JJcds3xfn",
		fx("-----END RSA ", "PRIVATE KEY-----"),
		"suffix",
	}, "\n")
	out, res := String(in)
	if strings.Contains(out, "MIIEowIBAAKCAQEA") {
		t.Errorf("private key material survived:\n%s", out)
	}
	if !strings.Contains(out, "prefix") || !strings.Contains(out, "suffix") {
		t.Errorf("surrounding text damaged:\n%s", out)
	}
	if res.Counts["private-key"] != 1 {
		t.Errorf("counts = %v, want private-key:1", res.Counts)
	}
}

// TestString_EnvSecretPreservesKeyName: the env-secret rule must keep
// the key and separator (so the reader knows WHAT was redacted) and
// replace only the value.
func TestString_EnvSecretPreservesKeyName(t *testing.T) {
	out, _ := String(fx("GITHUB_TOKEN", "=", "ghx_notarealshape_but_long_value"))
	if !strings.Contains(out, "GITHUB_TOKEN=") {
		t.Errorf("key name lost: %q", out)
	}
	if strings.Contains(out, "notarealshape") {
		t.Errorf("value survived: %q", out)
	}
}

// TestString_FalsePositives: shapes that LOOK high-entropy but are
// normal uta output must survive untouched — blob refs, git SHAs,
// UUIDs, ordinary base64-ish IDs that don't match a credential prefix.
func TestString_FalsePositives(t *testing.T) {
	cases := []string{
		"blob:9f86d081884c7d659a2feaa0c55ad015a3bf4f1b",   // blob ref
		"commit 5bac385f1c2d3e4a5b6c7d8e9f0a1b2c3d4e5f6a", // git sha
		"session 4f6c1a2b-3d4e-5f60-7182-93a4b5c6d7e8",    // uuid
		"the word secretive is fine",                      // 'secret' inside a word, no assignment
		"tokens=12430 approx $0.04",                       // token COUNT, not a token
	}
	for _, in := range cases {
		out, res := String(in)
		if out != in {
			t.Errorf("false positive: %q became %q (counts=%v)", in, out, res.Counts)
		}
	}
}

func TestString_EmptyAndClean(t *testing.T) {
	out, res := String("")
	if out != "" || res.Total() != 0 {
		t.Errorf("empty input: out=%q total=%d", out, res.Total())
	}
	clean := "perfectly ordinary text with no secrets at all"
	out, res = String(clean)
	if out != clean || res.Total() != 0 {
		t.Errorf("clean input modified: %q total=%d", out, res.Total())
	}
}

func TestResult_Merge(t *testing.T) {
	a := Result{Counts: map[string]int{"aws-key": 1}}
	b := Result{Counts: map[string]int{"aws-key": 2, "jwt": 1}}
	a.Merge(b)
	if a.Counts["aws-key"] != 3 || a.Counts["jwt"] != 1 {
		t.Errorf("merge wrong: %v", a.Counts)
	}
	if a.Total() != 4 {
		t.Errorf("total = %d, want 4", a.Total())
	}
}

func TestBytes_RoundTrip(t *testing.T) {
	out, res := Bytes([]byte("key " + fx("AKIA", "IOSFODNN7EXAMPLE") + " end"))
	if strings.Contains(string(out), fx("AKIA", "IOSFODNN7")) {
		t.Errorf("bytes leak: %s", out)
	}
	if res.Total() != 1 {
		t.Errorf("total = %d, want 1", res.Total())
	}
}
