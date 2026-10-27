package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/unleashtheagents/uta/internal/limits"
)

func TestCredentialFileEditing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "credentials.env")
	if err := writeCredential(path, "NEBIUS_API_KEY", "v1.first", false); err != nil {
		t.Fatal(err)
	}
	if err := writeCredential(path, "TAVILY_API_KEY", "tvly-x", false); err != nil {
		t.Fatal(err)
	}
	if err := writeCredential(path, "NEBIUS_API_KEY", "v1.second", false); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Errorf("credentials file mode = %o, want 600", st.Mode().Perm())
	}
	data, _ := os.ReadFile(path)
	text := string(data)
	if strings.Count(text, "NEBIUS_API_KEY=") != 1 || !strings.Contains(text, `export NEBIUS_API_KEY="v1.second"`) {
		t.Errorf("replace did not keep a single NEBIUS line:\n%s", text)
	}
	if !strings.Contains(text, `export TAVILY_API_KEY="tvly-x"`) || !strings.HasPrefix(text, "#") {
		t.Errorf("header or other keys lost:\n%s", text)
	}
	if err := writeCredential(path, "TAVILY_API_KEY", "", true); err != nil {
		t.Fatal(err)
	}
	data, _ = os.ReadFile(path)
	if strings.Contains(string(data), "TAVILY") {
		t.Errorf("unset left the key behind:\n%s", data)
	}
}

func TestCredentialVarForAndMask(t *testing.T) {
	if v, err := credentialVarFor("Nebius"); err != nil || v != "NEBIUS_API_KEY" {
		t.Errorf("nebius -> %q %v", v, err)
	}
	if v, err := credentialVarFor("MY_PROXY_TOKEN"); err != nil || v != "MY_PROXY_TOKEN" {
		t.Errorf("raw var -> %q %v", v, err)
	}
	if _, err := credentialVarFor("unknownprovider"); err == nil {
		t.Error("lower-case unknown name must be rejected")
	}
	if got := maskSecret("v1.nebius.abcdefghijklmnop"); got != "v1.n******mnop" {
		t.Errorf("mask = %q", got)
	}
	if got := maskSecret("short"); got != "*****" {
		t.Errorf("short mask = %q", got)
	}
}

func TestLimitsSetStatusCommands(t *testing.T) {
	home := t.TempDir()
	t.Setenv("UTA_HOME", home)

	root := newLimitsCmd()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{"set", "nebius", "--daily-usd", "$5", "--monthly-tokens", "1000000"})
	if err := root.Execute(); err != nil {
		t.Fatalf("set: %v\n%s", err, out.String())
	}
	cfg, err := limits.Load(limits.File(home))
	if err != nil || cfg.Providers["nebius"].DailyUSD != 5 || cfg.Providers["nebius"].MonthlyTokens != 1_000_000 {
		t.Fatalf("saved config = %+v err=%v", cfg, err)
	}

	// Spend recorded through the guard shows up in status and trips the cap.
	g := loadUsageGuard(home, nil)
	g.Guard.AfterCall(t.Context(), limits.Entry{Provider: "nebius", TokensIn: 500, TokensOut: 100, USDCents: 500})
	if err := g.BeforeCall(t.Context(), "nebius"); err == nil {
		t.Fatal("at the $5 cap the guard must refuse")
	}
	out.Reset()
	root = newLimitsCmd()
	root.SetOut(&out)
	root.SetArgs([]string{"status"})
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "BLOCKED") || !strings.Contains(out.String(), "$5.00/day") {
		t.Errorf("status output:\n%s", out.String())
	}

	out.Reset()
	root = newLimitsCmd()
	root.SetOut(&out)
	root.SetArgs([]string{"clear", "nebius"})
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	cfg, _ = limits.Load(limits.File(home))
	if _, ok := cfg.Providers["nebius"]; ok {
		t.Error("clear left caps behind")
	}
	if err := loadUsageGuard(home, nil).BeforeCall(t.Context(), "nebius"); err != nil {
		t.Errorf("uncapped after clear: %v", err)
	}
}
