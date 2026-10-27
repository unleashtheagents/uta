package cli

import (
	"bytes"
	"os"
	"strings"
	"testing"

	"github.com/unleashtheagents/uta/internal/limits"
	"github.com/unleashtheagents/uta/internal/paths"
)

func TestDefaultWorkerPrefersNebius(t *testing.T) {
	if got := defaultWorker([]string{"agy", "claude", "nebius"}); got != "nebius" {
		t.Errorf("got %q", got)
	}
	if got := defaultWorker([]string{"agy", "claude"}); got != "agy" {
		t.Errorf("got %q", got)
	}
	if got := defaultWorker(nil); got != "" {
		t.Errorf("got %q", got)
	}
}

func TestShouldOpenShell(t *testing.T) {
	if !shouldOpenShell(true, nil) {
		t.Error("interactive bare uta should open the shell")
	}
	if shouldOpenShell(false, nil) {
		t.Error("piped bare uta should print help")
	}
	if shouldOpenShell(true, []string{"x"}) {
		t.Error("args mean a command, not the shell")
	}
}

func TestApplySetupWritesKeysAndCap(t *testing.T) {
	home := t.TempDir()
	t.Setenv("NEBIUS_API_KEY", "")
	t.Setenv("TAVILY_API_KEY", "")
	if err := applySetup(home, setupAnswers{NebiusKey: "v1.nebius.test1234567890", TavilyKey: "tvly-test", DailyUSD: 7.5}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(paths.CredentialsFile(home))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `NEBIUS_API_KEY="v1.nebius.test1234567890"`) || !strings.Contains(string(data), `TAVILY_API_KEY="tvly-test"`) {
		t.Errorf("credentials:\n%s", data)
	}
	if os.Getenv("NEBIUS_API_KEY") != "v1.nebius.test1234567890" {
		t.Error("key must be live in the current process")
	}
	cfg, err := limits.Load(limits.File(home))
	if err != nil || cfg.Providers["nebius"].DailyUSD != 7.5 {
		t.Errorf("limits = %+v err=%v", cfg, err)
	}
	// Skipping everything leaves files untouched.
	home2 := t.TempDir()
	if err := applySetup(home2, setupAnswers{}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(paths.CredentialsFile(home2)); !os.IsNotExist(err) {
		t.Error("empty answers must not create a credentials file")
	}
}

func TestSetupCommandNonInteractive(t *testing.T) {
	home := t.TempDir()
	t.Setenv("UTA_HOME", home)
	t.Setenv("NEBIUS_API_KEY", "")
	cmd := newSetupCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"--nebius-key", "v1.nebius.abcdefghijk", "--daily-usd", "$3"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	cfg, _ := limits.Load(limits.File(home))
	if cfg.Providers["nebius"].DailyUSD != 3 {
		t.Errorf("cap = %+v", cfg.Providers["nebius"])
	}
	if !strings.Contains(out.String(), "setup saved") {
		t.Errorf("out = %s", out.String())
	}
}
