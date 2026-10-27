package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/unleashtheagents/uta/internal/provider/builtin"
)

func TestNebiusCmd_StatusModelsAndRoute(t *testing.T) {
	t.Setenv("NEBIUS_API_KEY", "test-nebius-key")
	t.Setenv("NEBIUS_MODEL", "ultra")

	// 1. status --json
	cmd := newNebiusCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"status", "--json"})
	if err := cmd.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("nebius status --json: %v", err)
	}
	var rep nebiusStatusReport
	if err := json.Unmarshal(out.Bytes(), &rep); err != nil {
		t.Fatalf("decode status json: %v\nraw=%s", err, out.String())
	}
	if !rep.Authenticated {
		t.Errorf("expected Authenticated=true")
	}
	if rep.ActiveModel != builtin.NebiusModelUltra {
		t.Errorf("ActiveModel = %q, want %q", rep.ActiveModel, builtin.NebiusModelUltra)
	}
	if len(rep.Models) != 5 {
		t.Errorf("len(Models) = %d, want 5", len(rep.Models))
	}

	// 2. models table
	out.Reset()
	cmd = newNebiusCmd()
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"models"})
	if err := cmd.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("nebius models: %v", err)
	}
	for _, want := range []string{"ultra", "super", "nano", "lightning", "vl", builtin.NebiusModelUltra} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("nebius models output missing %q:\n%s", want, out.String())
		}
	}

	// 3. route
	out.Reset()
	cmd = newNebiusCmd()
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"route", "design formal distributed consensus architecture"})
	if err := cmd.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("nebius route: %v", err)
	}
	if !strings.Contains(out.String(), "ultra-deep") || !strings.Contains(out.String(), builtin.NebiusModelUltra) {
		t.Errorf("nebius route output unexpected:\n%s", out.String())
	}
}

func TestSkillsCmd_ListAndShow(t *testing.T) {
	dir := chdirTo(t, t.TempDir())
	t.Setenv("HOME", t.TempDir())

	if err := os.MkdirAll(filepath.Join(dir, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "AGENTS.md"), []byte("# AGY Workspace Rules\nUse Nebius Nemotron 3.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	skillDir := filepath.Join(dir, ".agents", "skills", "nemotron-bench")
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte("---\nname: nemotron-bench\ndescription: Benchmark NVIDIA Nemotron models on Nebius\n---\nRun uta bench.\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	cmd := newSkillsCmd()
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"list"})
	if err := cmd.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("skills list: %v", err)
	}
	for _, want := range []string{"AGENTS.md", "nemotron-bench", "agy"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("skills list missing %q:\n%s", want, out.String())
		}
	}

	out.Reset()
	cmd = newSkillsCmd()
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"show", "nemotron-bench"})
	if err := cmd.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("skills show: %v", err)
	}
	if !strings.Contains(out.String(), "Run uta bench.") {
		t.Errorf("skills show missing skill body:\n%s", out.String())
	}
}

func TestShell_NebiusPlanCompactSkillsCommands(t *testing.T) {
	f := &fakeShellBackend{session: "deadbeef"}
	out, _, _ := dispatch(t, f, "/compact")
	if f.session != "" || !strings.Contains(out, "compacted conversation") {
		t.Errorf("/compact failed: session=%q out=%q", f.session, out)
	}

	out, _, _ = dispatch(t, f, "/plan refactor worker pool")
	if !strings.Contains(out, "90/10-hybrid") || !strings.Contains(out, builtin.NebiusModelSuper) {
		t.Errorf("/plan output unexpected: %s", out)
	}

	out, _, _ = dispatch(t, f, "/nebius ultra")
	if f.worker != "nebius" || f.model != "NEBIUS_MODEL="+builtin.NebiusModelUltra {
		t.Errorf("/nebius ultra failed: worker=%q model=%q out=%q", f.worker, f.model, out)
	}
}

func TestNebiusCmd_SearchAndPing(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/search":
			_, _ = w.Write([]byte(`{"query":"nemotron","answer":"Nemotron 3 on Nebius Token Factory","results":[{"title":"Nebius Token Factory","url":"https://nebius.com","content":"148 tok/s","score":0.99}]}`))
		case "/v1/models":
			_, _ = w.Write([]byte(`{"data":[{"id":"nvidia/Nemotron-3-Ultra-550b-a55b"},{"id":"nvidia/nemotron-3-super-120b-a12b"}]}`))
		case "/v1/chat/completions":
			_, _ = w.Write([]byte(`{"id":"c1","model":"nvidia/nemotron-3-super-120b-a12b","choices":[{"index":0,"message":{"role":"assistant","content":"Synthesized by Nemotron Super."},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5}}`))
		}
	}))
	defer srv.Close()

	t.Setenv("TAVILY_API_KEY", "tvly-test")
	t.Setenv("TAVILY_BASE_URL", srv.URL)
	t.Setenv("NEBIUS_API_KEY", "nebius-test")
	t.Setenv("NEBIUS_BASE_URL", srv.URL+"/v1")

	var out bytes.Buffer
	cmd := newNebiusCmd()
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"search", "nemotron"})
	if err := cmd.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("nebius search: %v", err)
	}
	if !strings.Contains(out.String(), "Nebius Token Factory") || !strings.Contains(out.String(), "Synthesized by Nemotron Super.") {
		t.Errorf("nebius search output unexpected:\n%s", out.String())
	}

	out.Reset()
	cmd = newNebiusCmd()
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"ping"})
	if err := cmd.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("nebius ping: %v", err)
	}
	if !strings.Contains(out.String(), "Nebius Token Factory ping OK") || !strings.Contains(out.String(), builtin.NebiusModelUltra) {
		t.Errorf("nebius ping output unexpected:\n%s", out.String())
	}
}
