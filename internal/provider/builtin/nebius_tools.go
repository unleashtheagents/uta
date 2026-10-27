package builtin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/unleashtheagents/uta/internal/provider"
	"github.com/unleashtheagents/uta/internal/tavily"
)

const maxNebiusToolRounds = 6

type nebiusToolSpec struct {
	Type     string             `json:"type"`
	Function nebiusToolFunction `json:"function"`
}

type nebiusToolFunction struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Parameters  map[string]any `json:"parameters"`
}

type nebiusToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
	// Index is only present on streamed deltas; it is cleared before a call
	// is replayed into history.
	Index *int `json:"index,omitempty"`
}

// DefaultNebiusTools returns the OpenAI-compatible function-calling tool
// definitions exposed to NVIDIA Nemotron models on Nebius Token Factory when
// agentic tool execution is enabled (`UTA_NEBIUS_AGENTIC=1` or `--preapprove` / `-y`).
func DefaultNebiusTools() []nebiusToolSpec {
	return []nebiusToolSpec{
		{
			Type: "function",
			Function: nebiusToolFunction{
				Name:        "read_file",
				Description: "Read the contents of a file inside the repository workspace.",
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"path": map[string]any{
							"type":        "string",
							"description": "Relative path to the file inside the workspace.",
						},
					},
					"required": []string{"path"},
				},
			},
		},
		{
			Type: "function",
			Function: nebiusToolFunction{
				Name:        "write_file",
				Description: "Create or overwrite a file inside the repository workspace.",
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"path": map[string]any{
							"type":        "string",
							"description": "Relative path to the file inside the workspace.",
						},
						"content": map[string]any{
							"type":        "string",
							"description": "Complete file contents to write.",
						},
					},
					"required": []string{"path", "content"},
				},
			},
		},
		{
			Type: "function",
			Function: nebiusToolFunction{
				Name:        "edit_file",
				Description: "Replace an exact target string with a replacement string in an existing workspace file.",
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"path": map[string]any{
							"type":        "string",
							"description": "Relative path to the file inside the workspace.",
						},
						"target": map[string]any{
							"type":        "string",
							"description": "Exact existing substring to replace.",
						},
						"replacement": map[string]any{
							"type":        "string",
							"description": "Replacement substring.",
						},
					},
					"required": []string{"path", "target", "replacement"},
				},
			},
		},
		{
			Type: "function",
			Function: nebiusToolFunction{
				Name:        "list_dir",
				Description: "List files and directories at a relative path inside the repository workspace.",
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"path": map[string]any{
							"type":        "string",
							"description": "Relative directory path (use '.' for workspace root).",
						},
					},
				},
			},
		},
		{
			Type: "function",
			Function: nebiusToolFunction{
				Name:        "run_command",
				Description: "Execute a shell command (such as `go test ./...` or `git diff`) inside the repository workspace.",
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"command": map[string]any{
							"type":        "string",
							"description": "Command line to execute in the workspace.",
						},
					},
					"required": []string{"command"},
				},
			},
		},
		{
			Type: "function",
			Function: nebiusToolFunction{
				Name:        "tavily_search",
				Description: "Search live web documentation, benchmarks, and technical references via the Tavily Search API.",
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"query": map[string]any{
							"type":        "string",
							"description": "Search query to execute against Tavily.",
						},
					},
					"required": []string{"query"},
				},
			},
		},
	}
}

// readOnlyNebiusToolNames is the default tool set for worker turns that were
// not pre-approved: the model can look at the repository and search the web
// but cannot change files or run commands. --pre-approve (any value) or
// UTA_NEBIUS_AGENTIC=1 unlocks the full set; UTA_NEBIUS_AGENTIC=0 disables
// tools entirely.
var readOnlyNebiusToolNames = map[string]bool{"read_file": true, "list_dir": true, "tavily_search": true}

// nebiusToolsFor returns the tool definitions to advertise for a call.
// Planner, synthesis, judge, compact and sentinel turns never get tools;
// they reason over text the engine already gathered.
func nebiusToolsFor(opts provider.RunOptions) []nebiusToolSpec {
	if v := resolveEnvKey(opts.Env, "UTA_NEBIUS_AGENTIC"); v != "" {
		if v == "0" || strings.EqualFold(v, "false") || strings.EqualFold(v, "off") {
			return nil
		}
		if v == "1" || strings.EqualFold(v, "true") {
			return DefaultNebiusTools()
		}
	}
	if len(opts.PreApproveTools) > 0 {
		return DefaultNebiusTools()
	}
	if opts.Role != "" && opts.Role != provider.RoleWorker {
		return nil
	}
	var out []nebiusToolSpec
	for _, t := range DefaultNebiusTools() {
		if readOnlyNebiusToolNames[t.Function.Name] {
			out = append(out, t)
		}
	}
	return out
}

func resolveSafeWorkdirPath(workdir, rel string) (string, error) {
	if strings.TrimSpace(workdir) == "" {
		var err error
		workdir, err = os.Getwd()
		if err != nil {
			return "", err
		}
	}
	absRoot, err := filepath.Abs(workdir)
	if err != nil {
		return "", err
	}
	cleaned := filepath.Clean(strings.TrimSpace(rel))
	if cleaned == "" {
		cleaned = "."
	}
	if filepath.IsAbs(cleaned) {
		return "", fmt.Errorf("absolute paths are not permitted: %s", rel)
	}
	target := filepath.Join(absRoot, cleaned)
	relCheck, err := filepath.Rel(absRoot, target)
	if err != nil || strings.HasPrefix(relCheck, "..") {
		return "", fmt.Errorf("path escapes workspace root: %s", rel)
	}
	return target, nil
}

func executeNebiusToolCall(ctx context.Context, call nebiusToolCall, opts provider.RunOptions) (string, bool) {
	var args map[string]any
	if strings.TrimSpace(call.Function.Arguments) != "" {
		if err := json.Unmarshal([]byte(call.Function.Arguments), &args); err != nil {
			return fmt.Sprintf("error parsing tool arguments: %v", err), true
		}
	}
	strArg := func(k string) string {
		if v, ok := args[k]; ok {
			if s, ok := v.(string); ok {
				return s
			}
		}
		return ""
	}

	switch call.Function.Name {
	case "read_file":
		target, err := resolveSafeWorkdirPath(opts.Workdir, strArg("path"))
		if err != nil {
			return fmt.Sprintf("read_file error: %v", err), true
		}
		data, err := os.ReadFile(target)
		if err != nil {
			return fmt.Sprintf("read_file error: %v", err), true
		}
		const maxRead = 64 * 1024
		if len(data) > maxRead {
			return string(data[:maxRead]) + "\n... [truncated]", false
		}
		return string(data), false

	case "write_file":
		target, err := resolveSafeWorkdirPath(opts.Workdir, strArg("path"))
		if err != nil {
			return fmt.Sprintf("write_file error: %v", err), true
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return fmt.Sprintf("write_file mkdir error: %v", err), true
		}
		content := strArg("content")
		if err := os.WriteFile(target, []byte(content), 0o644); err != nil {
			return fmt.Sprintf("write_file error: %v", err), true
		}
		return fmt.Sprintf("wrote %d bytes to %s", len(content), strArg("path")), false

	case "edit_file":
		target, err := resolveSafeWorkdirPath(opts.Workdir, strArg("path"))
		if err != nil {
			return fmt.Sprintf("edit_file error: %v", err), true
		}
		raw, err := os.ReadFile(target)
		if err != nil {
			return fmt.Sprintf("edit_file read error: %v", err), true
		}
		oldStr := strArg("target")
		newStr := strArg("replacement")
		if oldStr == "" || !strings.Contains(string(raw), oldStr) {
			return "edit_file error: target string not found in file", true
		}
		updated := strings.Replace(string(raw), oldStr, newStr, 1)
		if err := os.WriteFile(target, []byte(updated), 0o644); err != nil {
			return fmt.Sprintf("edit_file write error: %v", err), true
		}
		return fmt.Sprintf("edited %s", strArg("path")), false

	case "list_dir":
		rel := strArg("path")
		if rel == "" {
			rel = "."
		}
		target, err := resolveSafeWorkdirPath(opts.Workdir, rel)
		if err != nil {
			return fmt.Sprintf("list_dir error: %v", err), true
		}
		entries, err := os.ReadDir(target)
		if err != nil {
			return fmt.Sprintf("list_dir error: %v", err), true
		}
		var names []string
		for _, e := range entries {
			name := e.Name()
			if e.IsDir() {
				name += "/"
			}
			names = append(names, name)
		}
		return strings.Join(names, "\n"), false

	case "run_command":
		cmdStr := strings.TrimSpace(strArg("command"))
		if cmdStr == "" {
			return "run_command error: empty command", true
		}
		cmdCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		c := exec.CommandContext(cmdCtx, "sh", "-c", cmdStr)
		if opts.Workdir != "" {
			c.Dir = opts.Workdir
		}
		out, err := c.CombinedOutput()
		const maxOut = 32 * 1024
		outStr := string(out)
		if len(outStr) > maxOut {
			outStr = outStr[:maxOut] + "\n... [truncated]"
		}
		if err != nil {
			return fmt.Sprintf("command exited with error (%v):\n%s", err, outStr), true
		}
		if strings.TrimSpace(outStr) == "" {
			return "(exit 0, no output)", false
		}
		return outStr, false

	case "tavily_search":
		q := strings.TrimSpace(strArg("query"))
		if q == "" {
			return "tavily_search error: empty query", true
		}
		tvClient := &tavily.Client{
			APIKey:  resolveEnvKey(opts.Env, "TAVILY_API_KEY"),
			BaseURL: resolveEnvKey(opts.Env, "TAVILY_BASE_URL"),
		}
		sr, err := tvClient.Search(ctx, q, 5)
		if err != nil {
			return fmt.Sprintf("tavily_search error: %v", err), true
		}
		md := tavily.FormatMarkdown(sr)
		// Persist citations the same way `uta nebius search` does, so research
		// an agent did mid-task survives in the project's context directory.
		if opts.Workdir != "" {
			ctxDir := filepath.Join(opts.Workdir, ".uta", "context")
			if st, serr := os.Stat(ctxDir); serr == nil && st.IsDir() {
				if f, ferr := os.OpenFile(filepath.Join(ctxDir, "RESEARCH_DATABASE.md"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644); ferr == nil {
					_, _ = fmt.Fprintf(f, "\n\n<!-- tavily_search by %s at %s -->\n%s\n", "nemotron", time.Now().UTC().Format(time.RFC3339), md)
					_ = f.Close()
				}
			}
		}
		return md, false

	default:
		return fmt.Sprintf("unknown tool: %s", call.Function.Name), true
	}
}

// FetchModels queries GET /v1/models on Nebius Token Factory and returns the
// list of remote model IDs reported by the endpoint.
func (n *Nebius) FetchModels(ctx context.Context) ([]string, error) {
	apiKey, baseURL, _ := n.resolveConfig(nil)
	if apiKey == "" {
		return nil, fmt.Errorf("nebius: %w: NEBIUS_API_KEY is not set", provider.ErrAuth)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/models", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+apiKey)

	client := n.Client
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("nebius fetch models: %w: %v", provider.ErrTransport, err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
			return nil, fmt.Errorf("nebius models (%d): %w: %s", resp.StatusCode, provider.ErrAuth, strings.TrimSpace(string(raw)))
		}
		return nil, fmt.Errorf("nebius models (%d): %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	var payload struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil, errors.New("nebius models: invalid JSON response")
	}
	ids := make([]string, 0, len(payload.Data))
	for _, d := range payload.Data {
		if strings.TrimSpace(d.ID) != "" {
			ids = append(ids, d.ID)
		}
	}
	return ids, nil
}
