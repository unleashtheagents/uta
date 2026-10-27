package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/unleashtheagents/uta/internal/mcp"
	"github.com/unleashtheagents/uta/internal/provider"
)

// MCP tools for Nemotron. Servers come from the run's materialized MCP
// config (RunOptions.MCPConfigPath, written by the engine from the active
// mission profile) or, when that is empty, from <workdir>/.mcp.json in the
// shape Claude Code and Cursor use. Each server is launched once per
// process and reused by every turn; its tools are advertised to the model
// as mcp__<server>__<tool> and run through the same permissions gate and
// sentinel as the built-in tools. UTA_NEBIUS_MCP=0 turns this off.

const nebiusMCPPrefix = "mcp__"

type mcpServerSpec struct {
	Name    string
	Command string
	Args    []string
	Env     map[string]string
}

type mcpConn struct {
	spec   mcpServerSpec
	client *mcp.Client
	tools  []mcp.Tool
	err    error
}

// mcpConfigFile is the on-disk shape shared by .mcp.json and the engine's
// materialized config.
type mcpConfigFile struct {
	Servers map[string]struct {
		Command string            `json:"command"`
		Args    []string          `json:"args,omitempty"`
		Env     map[string]string `json:"env,omitempty"`
	} `json:"mcpServers"`
}

func loadMCPConfig(path string) ([]mcpServerSpec, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var cfg mcpConfigFile
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	var out []mcpServerSpec
	for name, s := range cfg.Servers {
		if strings.TrimSpace(name) == "" || strings.TrimSpace(s.Command) == "" {
			continue
		}
		env := make(map[string]string, len(s.Env))
		for k, v := range s.Env {
			env[k] = os.ExpandEnv(v)
		}
		out = append(out, mcpServerSpec{Name: name, Command: s.Command, Args: s.Args, Env: env})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// nebiusMCPServers resolves the server list for a call.
func nebiusMCPServers(opts provider.RunOptions) ([]mcpServerSpec, string) {
	if v := resolveEnvKey(opts.Env, "UTA_NEBIUS_MCP"); v == "0" || strings.EqualFold(v, "off") || strings.EqualFold(v, "false") {
		return nil, ""
	}
	if p := strings.TrimSpace(opts.MCPConfigPath); p != "" {
		specs, err := loadMCPConfig(p)
		if err == nil {
			return specs, p
		}
	}
	dir := opts.Workdir
	if dir == "" {
		// A run without an explicit workdir operates on the current
		// directory for every file tool; .mcp.json discovery must agree.
		dir, _ = os.Getwd()
	}
	if dir != "" {
		p := filepath.Join(dir, ".mcp.json")
		if _, err := os.Stat(p); err == nil {
			specs, err := loadMCPConfig(p)
			if err == nil {
				return specs, p
			}
		}
	}
	return nil, ""
}

func (s mcpServerSpec) key() string {
	return s.Name + "\x00" + s.Command + "\x00" + strings.Join(s.Args, "\x00")
}

// mcpConnect returns a live connection for the spec, launching the server
// on first use. Launch failures are cached too so a broken server is
// reported once per process, not once per turn.
func (n *Nebius) mcpConnect(spec mcpServerSpec) *mcpConn {
	n.mcpMu.Lock()
	defer n.mcpMu.Unlock()
	if n.mcp == nil {
		n.mcp = map[string]*mcpConn{}
	}
	if c, ok := n.mcp[spec.key()]; ok {
		return c
	}
	c := &mcpConn{spec: spec}
	n.mcp[spec.key()] = c
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	client, err := mcp.NewStdioClient(context.Background(), spec.Command, spec.Args, spec.Env)
	if err != nil {
		c.err = fmt.Errorf("launch: %w", err)
		return c
	}
	if err := client.Initialize(ctx); err != nil {
		_ = client.Close()
		c.err = fmt.Errorf("initialize: %w", err)
		return c
	}
	tools, err := client.ListTools(ctx)
	if err != nil {
		_ = client.Close()
		c.err = fmt.Errorf("tools/list: %w", err)
		return c
	}
	c.client = client
	c.tools = tools
	return c
}

// CloseMCP shuts down every MCP server this provider launched.
func (n *Nebius) CloseMCP() {
	n.mcpMu.Lock()
	defer n.mcpMu.Unlock()
	for k, c := range n.mcp {
		if c.client != nil {
			_ = c.client.Close()
		}
		delete(n.mcp, k)
	}
}

// nebiusMCPToolSpecs advertises every connected server's tools. It reports
// launches and failures once through events.
func (n *Nebius) nebiusMCPToolSpecs(opts provider.RunOptions, events chan<- provider.Event) []nebiusToolSpec {
	specs, source := nebiusMCPServers(opts)
	if len(specs) == 0 {
		return nil
	}
	var out []nebiusToolSpec
	for _, spec := range specs {
		c := n.mcpConnect(spec)
		n.mcpMu.Lock()
		announced := n.mcpAnnounced[spec.key()]
		if n.mcpAnnounced == nil {
			n.mcpAnnounced = map[string]bool{}
		}
		n.mcpAnnounced[spec.key()] = true
		n.mcpMu.Unlock()
		if c.err != nil {
			if events != nil && !announced {
				pl, _ := json.Marshal(map[string]any{"type": "mcp", "server": spec.Name, "error": c.err.Error(),
					"text": fmt.Sprintf("[nebius mcp] server %s unavailable: %v", spec.Name, c.err)})
				events <- provider.Event{Kind: provider.EventStdoutChunk, Timestamp: time.Now(), Payload: pl}
			}
			continue
		}
		if events != nil && !announced {
			names := make([]string, 0, len(c.tools))
			for _, t := range c.tools {
				names = append(names, t.Name)
			}
			pl, _ := json.Marshal(map[string]any{"type": "mcp", "server": spec.Name, "tools": names, "source": source,
				"text": fmt.Sprintf("[nebius mcp] server %s: %d tools (%s)", spec.Name, len(c.tools), strings.Join(names, ", "))})
			events <- provider.Event{Kind: provider.EventStdoutChunk, Timestamp: time.Now(), Payload: pl}
		}
		for _, t := range c.tools {
			params := map[string]any{"type": "object"}
			if len(t.InputSchema) > 0 {
				var schema map[string]any
				if json.Unmarshal(t.InputSchema, &schema) == nil && schema != nil {
					if _, ok := schema["type"]; !ok {
						schema["type"] = "object"
					}
					params = schema
				}
			}
			desc := strings.TrimSpace(t.Description)
			if desc == "" {
				desc = t.Name
			}
			out = append(out, nebiusToolSpec{Type: "function", Function: nebiusToolFunction{
				Name:        nebiusMCPToolName(spec.Name, t.Name),
				Description: fmt.Sprintf("[MCP server %s] %s", spec.Name, desc),
				Parameters:  params,
			}})
		}
	}
	return out
}

// nebiusMCPToolName namespaces a server tool the same way claude does, so
// one permissions rule (mcp__github__create_issue) covers both paths.
func nebiusMCPToolName(server, tool string) string {
	clean := func(s string) string {
		var b strings.Builder
		for _, r := range s {
			switch {
			case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-':
				b.WriteRune(r)
			default:
				b.WriteRune('_')
			}
		}
		return b.String()
	}
	return nebiusMCPPrefix + clean(server) + "__" + clean(tool)
}

// callMCPTool routes mcp__<server>__<tool> to the matching connection.
func (n *Nebius) callMCPTool(ctx context.Context, call nebiusToolCall, opts provider.RunOptions) nebiusToolOutcome {
	name := strings.TrimPrefix(call.Function.Name, nebiusMCPPrefix)
	server, tool, ok := strings.Cut(name, "__")
	if !ok {
		return nebiusToolOutcome{Output: "mcp error: malformed tool name " + call.Function.Name, IsErr: true}
	}
	specs, _ := nebiusMCPServers(opts)
	for _, spec := range specs {
		if nebiusMCPToolName(spec.Name, "x") != nebiusMCPPrefix+server+"__x" {
			continue
		}
		c := n.mcpConnect(spec)
		if c.err != nil {
			return nebiusToolOutcome{Output: fmt.Sprintf("mcp error: server %s unavailable: %v", spec.Name, c.err), IsErr: true}
		}
		var realName string
		for _, t := range c.tools {
			if nebiusMCPToolName(spec.Name, t.Name) == call.Function.Name {
				realName = t.Name
				break
			}
		}
		if realName == "" {
			return nebiusToolOutcome{Output: fmt.Sprintf("mcp error: server %s has no tool %s", spec.Name, tool), IsErr: true}
		}
		args := json.RawMessage(strings.TrimSpace(call.Function.Arguments))
		if len(args) == 0 || !json.Valid(args) {
			args = json.RawMessage("{}")
		}
		callCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
		defer cancel()
		res, err := c.client.CallTool(callCtx, realName, args)
		if err != nil {
			return nebiusToolOutcome{Output: fmt.Sprintf("mcp error: %s/%s: %v", spec.Name, realName, err), IsErr: true}
		}
		var parts []string
		for _, b := range res.Content {
			if strings.TrimSpace(b.Text) != "" {
				parts = append(parts, b.Text)
			}
		}
		out := strings.Join(parts, "\n")
		const maxOut = 48 * 1024
		if len(out) > maxOut {
			out = out[:maxOut] + "\n... [truncated]"
		}
		if out == "" {
			out = "(no content)"
		}
		return nebiusToolOutcome{Output: out, IsErr: res.IsError}
	}
	return nebiusToolOutcome{Output: "mcp error: no server named " + server + " in the MCP config", IsErr: true}
}
