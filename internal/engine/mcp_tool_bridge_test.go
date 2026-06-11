package engine

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/unleashtheagents/uta/internal/mcp"
	"github.com/unleashtheagents/uta/internal/profile"
)

// findRepoRoot walks up from the test's working directory looking for the
// nearest go.mod so subprocess-style tests can run `go build` from a known
// anchor regardless of which package directory `go test` cd'd into.
func findRepoRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", os.ErrNotExist
		}
		dir = parent
	}
}

func TestMCPAllowPatterns_SortsAndPrefixes(t *testing.T) {
	results := []MCPProbeResult{
		{
			Server: "gmail",
			Tools: []mcp.Tool{
				{Name: "send_email"},
				{Name: "list_emails"},
			},
		},
		{
			Server: "calendar",
			Tools: []mcp.Tool{
				{Name: "create_event"},
			},
		},
	}
	got := MCPAllowPatterns(results)
	want := []string{
		"mcp__calendar__create_event",
		"mcp__gmail__list_emails",
		"mcp__gmail__send_email",
	}
	if len(got) != len(want) {
		t.Fatalf("MCPAllowPatterns = %v, want %v", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestMCPAllowPatterns_SkipsFailedProbes(t *testing.T) {
	results := []MCPProbeResult{
		{Server: "broken", Err: errfmt("oauth missing")},
		{Server: "ok", Tools: []mcp.Tool{{Name: "ping"}}},
	}
	got := MCPAllowPatterns(results)
	if len(got) != 1 || got[0] != "mcp__ok__ping" {
		t.Errorf("got %v, want [mcp__ok__ping]", got)
	}
}

func TestMCPClaudeToolName(t *testing.T) {
	if got := MCPClaudeToolName("gmail", "send_email"); got != "mcp__gmail__send_email" {
		t.Errorf("got %q", got)
	}
}

func TestApplyMCPBridge_NilSafe(t *testing.T) {
	if got := ApplyMCPBridge(context.Background(), nil, nil); got != nil {
		t.Errorf("nil req/profile should produce nil results, got %v", got)
	}
	if got := ApplyMCPBridge(context.Background(), &RunRequest{}, nil); got != nil {
		t.Errorf("nil profile should produce nil results, got %v", got)
	}
	if got := ApplyMCPBridge(context.Background(), &RunRequest{}, &profile.MissionProfile{Name: "x"}); got != nil {
		t.Errorf("empty MCPServers should produce nil results, got %v", got)
	}
}

func TestApplyMCPBridge_BadServerProducesCleanError(t *testing.T) {
	// Acceptance criterion: "Without OAuth: clean error, no panic." We
	// approximate that by pointing the bridge at a non-existent binary —
	// the failure should be captured in MCPProbeResult.Err, not panic,
	// and the run request should be left untouched.
	req := &RunRequest{PreApproveTools: []string{"Read"}}
	p := &profile.MissionProfile{
		Name: "ops",
		MCPServers: []profile.ProfileMCPServer{
			{Name: "phantom", Command: "this-binary-does-not-exist-uta-mcp-test"},
		},
	}
	results := ApplyMCPBridge(context.Background(), req, p)
	if len(results) != 1 {
		t.Fatalf("expected one probe result, got %d", len(results))
	}
	if results[0].Err == nil {
		t.Fatal("expected probe to record an error for missing binary")
	}
	if len(req.PreApproveTools) != 1 || req.PreApproveTools[0] != "Read" {
		t.Errorf("failing probe must not mutate PreApproveTools, got %v", req.PreApproveTools)
	}
	if diag := FormatMCPProbeError(results[0]); !strings.HasPrefix(diag, "mcp[phantom]:") {
		t.Errorf("FormatMCPProbeError = %q", diag)
	}
}

func TestFormatMCPProbeError_EmptyOnSuccess(t *testing.T) {
	if got := FormatMCPProbeError(MCPProbeResult{Server: "ok"}); got != "" {
		t.Errorf("expected empty string on success, got %q", got)
	}
}

func TestMergePatterns_DedupesPreservingOrder(t *testing.T) {
	a := []string{"Read", "Edit"}
	b := []string{"Read", "mcp__gmail__list", "Edit", "mcp__gmail__send"}
	got := mergePatterns(a, b)
	want := []string{"Read", "Edit", "mcp__gmail__list", "mcp__gmail__send"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestWriteMCPConfig_OmitsFailedProbes(t *testing.T) {
	servers := []profile.ProfileMCPServer{
		{Name: "gmail", Command: "npx", Args: []string{"-y", "gmail-mcp"}, Env: map[string]string{"OAUTH": "${UTA_TEST_OAUTH}"}},
		{Name: "broken", Command: "no-such-bin"},
	}
	results := []MCPProbeResult{
		{Server: "gmail", Tools: []mcp.Tool{{Name: "list"}}},
		{Server: "broken", Err: errfmt("launch: nope")},
	}
	t.Setenv("UTA_TEST_OAUTH", "/tmp/oauth.json")
	path, err := writeMCPConfig(servers, results, os.ExpandEnv)
	if err != nil {
		t.Fatalf("writeMCPConfig: %v", err)
	}
	if path == "" {
		t.Fatal("expected path, got empty")
	}
	defer os.Remove(path)

	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read materialized config: %v", err)
	}
	var doc struct {
		MCPServers map[string]struct {
			Command string            `json:"command"`
			Args    []string          `json:"args"`
			Env     map[string]string `json:"env"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, body)
	}
	if _, ok := doc.MCPServers["broken"]; ok {
		t.Errorf("failed-probe server must not appear in config: %s", body)
	}
	gmail, ok := doc.MCPServers["gmail"]
	if !ok {
		t.Fatalf("gmail entry missing: %s", body)
	}
	if gmail.Command != "npx" || len(gmail.Args) != 2 || gmail.Args[1] != "gmail-mcp" {
		t.Errorf("gmail command/args wrong: %+v", gmail)
	}
	if gmail.Env["OAUTH"] != "/tmp/oauth.json" {
		t.Errorf("env not expanded: %v", gmail.Env)
	}
}

func TestWriteMCPConfig_AllFailedReturnsEmpty(t *testing.T) {
	servers := []profile.ProfileMCPServer{
		{Name: "a", Command: "x"},
		{Name: "b", Command: "y"},
	}
	results := []MCPProbeResult{
		{Server: "a", Err: errfmt("nope")},
		{Server: "b", Err: errfmt("nope")},
	}
	path, err := writeMCPConfig(servers, results, nil)
	if err != nil {
		t.Fatalf("writeMCPConfig: %v", err)
	}
	if path != "" {
		os.Remove(path)
		t.Errorf("expected empty path when no probe succeeded, got %q", path)
	}
}

func TestApplyMCPBridge_PopulatesConfigPathOnSuccessfulProbe(t *testing.T) {
	// Build a stub MCP server binary on the fly so the bridge has a real
	// process to probe. Reuses tests/mcp_stub via `go run`. If `go` is
	// unavailable we skip — this is the integration counterpart to the
	// allow-pattern unit tests above.
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go toolchain not on PATH; skipping bridge integration test")
	}
	root, err := findRepoRoot()
	if err != nil {
		t.Skipf("repo root not found: %v", err)
	}
	tmp := t.TempDir()
	bin := filepath.Join(tmp, "mcp_stub_bin")
	build := exec.Command("go", "build", "-o", bin, "./tests/mcp_stub")
	build.Dir = root
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build stub: %v\n%s", err, out)
	}

	req := &RunRequest{}
	p := &profile.MissionProfile{
		Name: "ops",
		MCPServers: []profile.ProfileMCPServer{
			{Name: "stub", Command: bin},
		},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	results := ApplyMCPBridge(ctx, req, p)
	if len(results) != 1 || results[0].Err != nil {
		t.Fatalf("expected one successful probe, got %+v", results)
	}
	if req.MCPConfigPath == "" {
		t.Fatal("expected req.MCPConfigPath to be populated on successful probe")
	}
	defer os.Remove(req.MCPConfigPath)
	body, err := os.ReadFile(req.MCPConfigPath)
	if err != nil {
		t.Fatalf("read materialized config: %v", err)
	}
	if !strings.Contains(string(body), `"stub"`) || !strings.Contains(string(body), bin) {
		t.Errorf("materialized config missing server entry: %s", body)
	}
}

// TestApplyMCPBridgeToResume_HappyPath verifies the resume-shaped helper
// mirrors ApplyMCPBridge: a successful probe materializes the config and
// stores the path on ResumeRequest.MCPConfigPath, so a resumed turn sees
// the same toolset a fresh run would.
func TestApplyMCPBridgeToResume_HappyPath(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go toolchain not on PATH; skipping stub-build test")
	}
	root, err := findRepoRoot()
	if err != nil {
		t.Skipf("repo root not found: %v", err)
	}
	tmp := t.TempDir()
	bin := filepath.Join(tmp, "mcp_stub_bin")
	build := exec.Command("go", "build", "-o", bin, "./tests/mcp_stub")
	build.Dir = root
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build stub: %v\n%s", err, out)
	}
	req := &ResumeRequest{}
	p := &profile.MissionProfile{
		Name:       "ops",
		MCPServers: []profile.ProfileMCPServer{{Name: "stub", Command: bin}},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	results := ApplyMCPBridgeToResume(ctx, req, p)
	if len(results) != 1 || results[0].Err != nil {
		t.Fatalf("expected one successful probe, got %+v", results)
	}
	if req.MCPConfigPath == "" {
		t.Fatal("expected ResumeRequest.MCPConfigPath populated")
	}
	defer os.Remove(req.MCPConfigPath)
}

// TestApplyMCPBridgeToReflector_HappyPath mirrors the resume test for
// the reflector path. Audit critics run through the same provider call
// the supervisor uses for Run, so MCP wiring must reach them too.
func TestApplyMCPBridgeToReflector_HappyPath(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go toolchain not on PATH; skipping stub-build test")
	}
	root, err := findRepoRoot()
	if err != nil {
		t.Skipf("repo root not found: %v", err)
	}
	tmp := t.TempDir()
	bin := filepath.Join(tmp, "mcp_stub_bin")
	build := exec.Command("go", "build", "-o", bin, "./tests/mcp_stub")
	build.Dir = root
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build stub: %v\n%s", err, out)
	}
	req := &ReflectorRequest{}
	p := &profile.MissionProfile{
		Name:       "audit",
		MCPServers: []profile.ProfileMCPServer{{Name: "stub", Command: bin}},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	results := ApplyMCPBridgeToReflector(ctx, req, p)
	if len(results) != 1 || results[0].Err != nil {
		t.Fatalf("expected one successful probe, got %+v", results)
	}
	if req.MCPConfigPath == "" {
		t.Fatal("expected ReflectorRequest.MCPConfigPath populated")
	}
	defer os.Remove(req.MCPConfigPath)
}

// TestProbeAndWriteMCPConfig_NilProfile verifies the low-level helper
// is a safe no-op when called with nil — the contract callers in other
// packages (improve/loop) rely on.
func TestProbeAndWriteMCPConfig_NilProfile(t *testing.T) {
	cfg, patterns, probes := ProbeAndWriteMCPConfig(context.Background(), nil)
	if cfg != "" || patterns != nil || probes != nil {
		t.Errorf("nil profile: expected zero-values, got cfg=%q patterns=%v probes=%v", cfg, patterns, probes)
	}
}

func TestApplyMCPBridge_BadProbeLeavesConfigPathEmpty(t *testing.T) {
	req := &RunRequest{}
	p := &profile.MissionProfile{
		Name: "ops",
		MCPServers: []profile.ProfileMCPServer{
			{Name: "phantom", Command: "this-binary-does-not-exist-uta-mcp-bridge-test"},
		},
	}
	results := ApplyMCPBridge(context.Background(), req, p)
	if len(results) != 1 || results[0].Err == nil {
		t.Fatalf("expected one failing probe, got %+v", results)
	}
	if req.MCPConfigPath != "" {
		os.Remove(req.MCPConfigPath)
		t.Errorf("expected empty MCPConfigPath when every probe failed, got %q", req.MCPConfigPath)
	}
}

func errfmt(s string) error { return &simpleErr{s} }

type simpleErr struct{ s string }

func (e *simpleErr) Error() string { return e.s }
