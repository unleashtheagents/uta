package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/unleashtheagents/uta/internal/mcp"
	"github.com/unleashtheagents/uta/internal/profile"
)

// MCPProbeTimeout is the per-server budget for launch + handshake +
// tools/list during the bridge probe. Real-world Gmail / Slack servers
// can take a few seconds to spin up their OAuth flow before they answer
// initialize; 10s leaves room without making a misconfigured server
// stall the whole run.
const MCPProbeTimeout = 10 * time.Second

// MCPProbeResult is the per-server outcome of LoadMCPTools. Each entry
// is either a success (Tools is populated, Err is nil) or a failure
// (Err is non-nil). Both flavors carry the server name so callers can
// surface partial results to the operator.
type MCPProbeResult struct {
	Server string
	Tools  []mcp.Tool
	Err    error
}

// LoadMCPTools launches each server in turn, performs the MCP handshake,
// asks for its tool catalog, and shuts the connection down. Servers are
// probed sequentially to keep stderr ordering readable for operators —
// the worst case is a handful of servers per profile, so the latency
// cost is acceptable.
//
// A failure on one server is recorded as MCPProbeResult.Err and does NOT
// abort the loop: a Gmail server missing its OAuth token should produce
// a clean error for the Gmail entry while leaving (say) a Calendar entry
// still operational. This matches the "Without OAuth: clean error, no
// panic" acceptance criterion.
//
// envExpand, when non-nil, is applied to each ProfileMCPServer.Env value
// before launch. Pass os.ExpandEnv to honor ${VAR} references that the
// profile YAML carries (e.g. ${UTA_OPS_GMAIL_OAUTH}); pass nil for
// verbatim env passthrough.
func LoadMCPTools(ctx context.Context, servers []profile.ProfileMCPServer, envExpand func(string) string) []MCPProbeResult {
	out := make([]MCPProbeResult, 0, len(servers))
	for _, srv := range servers {
		out = append(out, probeOne(ctx, srv, envExpand))
	}
	return out
}

func probeOne(ctx context.Context, srv profile.ProfileMCPServer, envExpand func(string) string) MCPProbeResult {
	res := MCPProbeResult{Server: srv.Name}

	expanded := srv.Env
	if envExpand != nil && len(srv.Env) > 0 {
		expanded = make(map[string]string, len(srv.Env))
		for k, v := range srv.Env {
			expanded[k] = envExpand(v)
		}
	}

	probeCtx, cancel := context.WithTimeout(ctx, MCPProbeTimeout)
	defer cancel()

	client, err := mcp.NewStdioClient(probeCtx, srv.Command, srv.Args, expanded)
	if err != nil {
		res.Err = fmt.Errorf("launch: %w", err)
		return res
	}
	// Best-effort close; we don't propagate close errors over a probe
	// failure that's already been recorded.
	defer func() { _ = client.Close() }()

	if err := client.Initialize(probeCtx); err != nil {
		res.Err = fmt.Errorf("initialize: %w", err)
		return res
	}
	tools, err := client.ListTools(probeCtx)
	if err != nil {
		res.Err = fmt.Errorf("tools/list: %w", err)
		return res
	}
	res.Tools = tools
	return res
}

// MCPAllowPatterns turns a slice of probe results into the
// "mcp__<server>__<tool>" patterns claude's --allowedTools flag expects.
// Failed probes contribute no patterns. The output is sorted for
// deterministic --pre-approve composition.
func MCPAllowPatterns(results []MCPProbeResult) []string {
	var patterns []string
	for _, r := range results {
		if r.Err != nil {
			continue
		}
		for _, t := range r.Tools {
			patterns = append(patterns, MCPClaudeToolName(r.Server, t.Name))
		}
	}
	sort.Strings(patterns)
	return patterns
}

// MCPClaudeToolName encodes a (server, tool) pair in claude's MCP tool
// namespacing: "mcp__<server>__<tool>". Exported so the CLI's
// diagnostics ("here's what was pre-approved") can render the same
// strings the bridge installs.
func MCPClaudeToolName(server, tool string) string {
	return "mcp__" + server + "__" + tool
}

// ProbeAndWriteMCPConfig is the lowest-level helper: it probes every
// server in p.MCPServers, materializes a `.mcp.json`-style config under
// os.TempDir for the ones that succeeded, and returns the path plus the
// allow-patterns and per-server probe outcomes.
//
// Use this when the caller has a request struct the ApplyMCPBridge*
// family doesn't know about (e.g. improve.LoopRequest, which lives in
// a different package). The caller is responsible for:
//   - merging the returned PreApprovePatterns into its own pre-approve list
//   - storing MCPConfigPath on its request
//   - removing the temp file with os.Remove after the run completes
//
// Returns ("", nil, nil) when p is nil or has no MCPServers.
func ProbeAndWriteMCPConfig(ctx context.Context, p *profile.MissionProfile) (mcpConfigPath string, preApprovePatterns []string, probes []MCPProbeResult) {
	if p == nil || len(p.MCPServers) == 0 {
		return "", nil, nil
	}
	probes = LoadMCPTools(ctx, p.MCPServers, os.ExpandEnv)
	preApprovePatterns = MCPAllowPatterns(probes)
	if path, err := writeMCPConfig(p.MCPServers, probes, os.ExpandEnv); err == nil {
		mcpConfigPath = path
	}
	return mcpConfigPath, preApprovePatterns, probes
}

// ApplyMCPBridgeToResume mirrors ApplyMCPBridge for ResumeRequest. Same
// probe → write `.mcp.json` → set MCPConfigPath pipeline; provided so
// `uta resume`, `uta improve`, and `uta audit` get the same MCP wiring
// `uta run` gets without each caller having to reimplement the bridge
// for their specific request shape.
//
// Returns the per-server probe outcomes (callers may surface failures as
// stderr warnings). No-op when req or p is nil, or p.MCPServers is empty.
// The caller owns the temp file: `defer os.Remove(req.MCPConfigPath)`.
func ApplyMCPBridgeToResume(ctx context.Context, req *ResumeRequest, p *profile.MissionProfile) []MCPProbeResult {
	if req == nil || p == nil || len(p.MCPServers) == 0 {
		return nil
	}
	results := LoadMCPTools(ctx, p.MCPServers, os.ExpandEnv)
	patterns := MCPAllowPatterns(results)
	if len(patterns) > 0 {
		req.PreApproveTools = mergePatterns(req.PreApproveTools, patterns)
	}
	if path, err := writeMCPConfig(p.MCPServers, results, os.ExpandEnv); err == nil && path != "" {
		req.MCPConfigPath = path
	}
	return results
}

// ApplyMCPBridgeToReflector mirrors ApplyMCPBridge for ReflectorRequest.
// Audit mode runs critics through the same provider call path as Run, so
// it benefits from the same MCP wiring (e.g. an audit profile that
// declares an MCP server gives critics access to it).
func ApplyMCPBridgeToReflector(ctx context.Context, req *ReflectorRequest, p *profile.MissionProfile) []MCPProbeResult {
	if req == nil || p == nil || len(p.MCPServers) == 0 {
		return nil
	}
	results := LoadMCPTools(ctx, p.MCPServers, os.ExpandEnv)
	patterns := MCPAllowPatterns(results)
	if len(patterns) > 0 {
		req.PreApproveTools = mergePatterns(req.PreApproveTools, patterns)
	}
	if path, err := writeMCPConfig(p.MCPServers, results, os.ExpandEnv); err == nil && path != "" {
		req.MCPConfigPath = path
	}
	return results
}

// ApplyMCPBridge probes each server declared in p.MCPServers and appends
// the resulting allow-patterns to req.PreApproveTools. Duplicate
// patterns are de-duplicated so a profile that also lists matching
// entries under allowed_tools won't end up with double entries in the
// final --allowedTools string.
//
// For every server that probed successfully, the bridge also materializes
// a `.mcp.json`-style file under os.TempDir and writes its path to
// req.MCPConfigPath. Providers with CapMCP (claude today) translate that
// path into their own CLI flag (`--mcp-config`) so the worker can actually
// connect to and call the same servers uta probed. The caller is
// responsible for removing the file after the run — callers that want to
// keep the temp dir clean should `defer os.Remove(req.MCPConfigPath)`.
//
// The returned slice is the per-server probe outcomes — callers may log
// failed probes to stderr or emit them as trajectory warnings, but the
// bridge does NOT abort the run on any single failure (Gmail without
// OAuth still leaves Calendar usable). When every probe fails the file
// is not written and req.MCPConfigPath stays empty.
//
// No-op when req or p is nil, or p.MCPServers is empty.
func ApplyMCPBridge(ctx context.Context, req *RunRequest, p *profile.MissionProfile) []MCPProbeResult {
	if req == nil || p == nil || len(p.MCPServers) == 0 {
		return nil
	}
	results := LoadMCPTools(ctx, p.MCPServers, os.ExpandEnv)
	patterns := MCPAllowPatterns(results)
	if len(patterns) > 0 {
		req.PreApproveTools = mergePatterns(req.PreApproveTools, patterns)
	}
	if path, err := writeMCPConfig(p.MCPServers, results, os.ExpandEnv); err == nil && path != "" {
		req.MCPConfigPath = path
	}
	return results
}

// writeMCPConfig serializes the subset of servers that probed successfully
// into a temp `.mcp.json` file claude (and any other provider that speaks
// the canonical shape) can consume via `--mcp-config`. Env values are run
// through envExpand so ${VAR} references on the profile resolve to the
// parent process's environment at probe time, matching what the bridge
// itself used when it launched the server.
//
// Returns ("", nil) when no server probed successfully — the caller should
// leave req.MCPConfigPath empty rather than pointing claude at a stub.
// A write error is propagated so callers can surface it; callers that
// don't care about the failure case may ignore it (the bridge does, since
// failure here is best-effort and shouldn't abort the run).
func writeMCPConfig(servers []profile.ProfileMCPServer, results []MCPProbeResult, envExpand func(string) string) (string, error) {
	ok := make(map[string]bool, len(results))
	for _, r := range results {
		if r.Err == nil {
			ok[r.Server] = true
		}
	}
	type mcpEntry struct {
		Command string            `json:"command"`
		Args    []string          `json:"args,omitempty"`
		Env     map[string]string `json:"env,omitempty"`
	}
	entries := make(map[string]mcpEntry, len(servers))
	for _, s := range servers {
		if !ok[s.Name] {
			continue
		}
		env := s.Env
		if envExpand != nil && len(s.Env) > 0 {
			env = make(map[string]string, len(s.Env))
			for k, v := range s.Env {
				env[k] = envExpand(v)
			}
		}
		entries[s.Name] = mcpEntry{Command: s.Command, Args: s.Args, Env: env}
	}
	if len(entries) == 0 {
		return "", nil
	}
	body, err := json.MarshalIndent(map[string]any{"mcpServers": entries}, "", "  ")
	if err != nil {
		return "", err
	}
	f, err := os.CreateTemp("", "uta-mcp-*.json")
	if err != nil {
		return "", err
	}
	if _, err := f.Write(body); err != nil {
		_ = f.Close()
		_ = os.Remove(f.Name())
		return "", err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(f.Name())
		return "", err
	}
	return f.Name(), nil
}

// mergePatterns appends new entries from b that are not already present
// in a, preserving a's order. Used to splice MCP tool patterns into a
// user-supplied --pre-approve list without losing the existing entries.
func mergePatterns(a, b []string) []string {
	if len(b) == 0 {
		return a
	}
	have := make(map[string]struct{}, len(a))
	for _, s := range a {
		have[s] = struct{}{}
	}
	out := make([]string, 0, len(a)+len(b))
	out = append(out, a...)
	for _, s := range b {
		if _, ok := have[s]; ok {
			continue
		}
		have[s] = struct{}{}
		out = append(out, s)
	}
	return out
}

// FormatMCPProbeError condenses a probe result into a single-line
// diagnostic suitable for stderr / trajectory events. Returns "" when
// the probe succeeded — callers can if-check the result instead of
// branching on Err themselves.
func FormatMCPProbeError(r MCPProbeResult) string {
	if r.Err == nil {
		return ""
	}
	msg := r.Err.Error()
	msg = strings.ReplaceAll(msg, "\n", " ")
	return "mcp[" + r.Server + "]: " + msg
}
