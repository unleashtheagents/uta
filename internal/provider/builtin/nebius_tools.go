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
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/unleashtheagents/uta/internal/provider"
	"github.com/unleashtheagents/uta/internal/tavily"
	"github.com/unleashtheagents/uta/internal/textdiff"
)

// maxNebiusToolRounds caps tool-calling rounds per turn; UTA_NEBIUS_MAX_TOOL_ROUNDS
// overrides it. When the cap is hit the model gets one closing turn without
// tools so the user always receives a report of what was done.
const maxNebiusToolRounds = 12

func resolveNebiusToolRounds(env []string) int {
	if v := resolveEnvKey(env, "UTA_NEBIUS_MAX_TOOL_ROUNDS"); v != "" {
		var n int
		if _, err := fmt.Sscanf(v, "%d", &n); err == nil && n > 0 {
			return n
		}
	}
	return maxNebiusToolRounds
}

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
				Name:        "search_files",
				Description: "Search file contents in the workspace with a regular expression (like grep -rn). Returns path:line: text matches. Use it to find where a symbol, string or error is defined or used before reading whole files.",
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"pattern": map[string]any{
							"type":        "string",
							"description": "Go/RE2 regular expression to search for.",
						},
						"path": map[string]any{
							"type":        "string",
							"description": "Relative directory or file to search (default: workspace root).",
						},
						"glob": map[string]any{
							"type":        "string",
							"description": "Only search files whose name matches this glob, e.g. *.go or *_test.go.",
						},
						"max_results": map[string]any{
							"type":        "integer",
							"description": "Maximum matching lines to return (default 100, max 500).",
						},
					},
					"required": []string{"pattern"},
				},
			},
		},
		{
			Type: "function",
			Function: nebiusToolFunction{
				Name:        "glob",
				Description: "Find files by name pattern, recursively. Supports * ? and ** (any directories), e.g. **/*.go, cmd/**/main.go, *.md. Returns relative paths sorted by modification time, newest first.",
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"pattern": map[string]any{
							"type":        "string",
							"description": "Glob pattern relative to the workspace root.",
						},
						"max_results": map[string]any{
							"type":        "integer",
							"description": "Maximum paths to return (default 200).",
						},
					},
					"required": []string{"pattern"},
				},
			},
		},
		{
			Type: "function",
			Function: nebiusToolFunction{
				Name:        "tavily_search",
				Description: "Search live web documentation, benchmarks, and technical references via the Tavily Search API. Returns ranked sources with snippets; call tavily_extract to read a source in full.",
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
		{
			Type: "function",
			Function: nebiusToolFunction{
				Name:        "tavily_extract",
				Description: "Fetch the readable text of one or more web pages (documentation, changelogs, issues) via the Tavily Extract API. Use it after tavily_search to read a source instead of guessing from its snippet.",
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"urls": map[string]any{
							"type":        "array",
							"items":       map[string]any{"type": "string"},
							"description": "Up to 5 absolute URLs to read.",
						},
					},
					"required": []string{"urls"},
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
var readOnlyNebiusToolNames = map[string]bool{
	"read_file": true, "list_dir": true, "search_files": true, "glob": true,
	"tavily_search": true, "tavily_extract": true,
}

// nebiusToolsFor returns the tool definitions to advertise for a call.
// Planner, synthesis, judge, compact and sentinel turns never get tools;
// they reason over text the engine already gathered. When the caller
// installed a ToolGate the full set is advertised and the gate decides
// per call (allow / ask / deny from .uta/permissions.yaml).
func nebiusToolsFor(opts provider.RunOptions) []nebiusToolSpec {
	if v := resolveEnvKey(opts.Env, "UTA_NEBIUS_AGENTIC"); v != "" {
		if v == "0" || strings.EqualFold(v, "false") || strings.EqualFold(v, "off") {
			return nil
		}
		if v == "1" || strings.EqualFold(v, "true") {
			return DefaultNebiusTools()
		}
	}
	if opts.Role != "" && opts.Role != provider.RoleWorker {
		return nil
	}
	if len(opts.PreApproveTools) > 0 || opts.ToolGate != nil {
		return DefaultNebiusTools()
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

// nebiusToolSummary extracts the one argument string that rules and humans
// care about for a call: the command, the path, the query or the pattern.
func nebiusToolSummary(call nebiusToolCall) string {
	var args map[string]any
	if json.Unmarshal([]byte(call.Function.Arguments), &args) != nil {
		return ""
	}
	for _, k := range []string{"command", "path", "query", "pattern"} {
		if v, ok := args[k].(string); ok && strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	if v, ok := args["urls"].([]any); ok {
		var parts []string
		for _, u := range v {
			if s, ok := u.(string); ok {
				parts = append(parts, s)
			}
		}
		return strings.Join(parts, " ")
	}
	return ""
}

// nebiusToolOutcome is what a tool execution produced: the text the model
// sees plus, for file changes, a unified diff for the trajectory and the
// progress renderer.
type nebiusToolOutcome struct {
	Output string
	IsErr  bool
	Diff   string
	Path   string
}

func executeNebiusToolCall(ctx context.Context, call nebiusToolCall, opts provider.RunOptions) (string, bool) {
	o := executeNebiusTool(ctx, call, opts)
	return o.Output, o.IsErr
}

var nebiusSkipDirs = map[string]bool{".git": true, "node_modules": true, ".uta": true, "vendor": true, "dist": true, "build": true, "__pycache__": true}

// globToPathRegexp turns a glob with *, ? and ** into a regexp over
// slash-separated relative paths. ** matches across directories; a single
// * stays inside one path segment.
func globToPathRegexp(glob string) (*regexp.Regexp, error) {
	glob = strings.TrimPrefix(filepath.ToSlash(filepath.Clean(glob)), "./")
	var b strings.Builder
	b.WriteString("^")
	rs := []rune(glob)
	for i := 0; i < len(rs); i++ {
		switch rs[i] {
		case '*':
			if i+1 < len(rs) && rs[i+1] == '*' {
				i++
				if i+1 < len(rs) && rs[i+1] == '/' {
					i++
					b.WriteString("(?:.*/)?")
				} else {
					b.WriteString(".*")
				}
			} else {
				b.WriteString("[^/]*")
			}
		case '?':
			b.WriteString("[^/]")
		default:
			b.WriteString(regexp.QuoteMeta(string(rs[i])))
		}
	}
	b.WriteString("$")
	return regexp.Compile(b.String())
}

func intArg(args map[string]any, k string, def int) int {
	if v, ok := args[k]; ok {
		switch n := v.(type) {
		case float64:
			return int(n)
		case int:
			return n
		case string:
			var i int
			if _, err := fmt.Sscanf(n, "%d", &i); err == nil {
				return i
			}
		}
	}
	return def
}

func executeNebiusTool(ctx context.Context, call nebiusToolCall, opts provider.RunOptions) nebiusToolOutcome {
	out, isErr, diff, path := executeNebiusToolInner(ctx, call, opts)
	return nebiusToolOutcome{Output: out, IsErr: isErr, Diff: diff, Path: path}
}

func executeNebiusToolInner(ctx context.Context, call nebiusToolCall, opts provider.RunOptions) (string, bool, string, string) {
	var args map[string]any
	if strings.TrimSpace(call.Function.Arguments) != "" {
		if err := json.Unmarshal([]byte(call.Function.Arguments), &args); err != nil {
			return fmt.Sprintf("error parsing tool arguments: %v", err), true, "", ""
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
			return fmt.Sprintf("read_file error: %v", err), true, "", ""
		}
		data, err := os.ReadFile(target)
		if err != nil {
			return fmt.Sprintf("read_file error: %v", err), true, "", ""
		}
		const maxRead = 64 * 1024
		if len(data) > maxRead {
			return string(data[:maxRead]) + "\n... [truncated]", false, "", ""
		}
		return string(data), false, "", ""

	case "write_file":
		target, err := resolveSafeWorkdirPath(opts.Workdir, strArg("path"))
		if err != nil {
			return fmt.Sprintf("write_file error: %v", err), true, "", ""
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return fmt.Sprintf("write_file mkdir error: %v", err), true, "", ""
		}
		content := strArg("content")
		before, _ := os.ReadFile(target)
		if err := os.WriteFile(target, []byte(content), 0o644); err != nil {
			return fmt.Sprintf("write_file error: %v", err), true, "", ""
		}
		return fmt.Sprintf("wrote %d bytes to %s", len(content), strArg("path")), false,
			textdiff.Unified(strArg("path"), string(before), content), strArg("path")

	case "edit_file":
		target, err := resolveSafeWorkdirPath(opts.Workdir, strArg("path"))
		if err != nil {
			return fmt.Sprintf("edit_file error: %v", err), true, "", ""
		}
		raw, err := os.ReadFile(target)
		if err != nil {
			return fmt.Sprintf("edit_file read error: %v", err), true, "", ""
		}
		oldStr := strArg("target")
		newStr := strArg("replacement")
		if oldStr == "" || !strings.Contains(string(raw), oldStr) {
			return "edit_file error: target string not found in file", true, "", ""
		}
		if strings.Count(string(raw), oldStr) > 1 {
			return fmt.Sprintf("edit_file error: target string occurs %d times; include more surrounding context so it is unique", strings.Count(string(raw), oldStr)), true, "", ""
		}
		updated := strings.Replace(string(raw), oldStr, newStr, 1)
		if err := os.WriteFile(target, []byte(updated), 0o644); err != nil {
			return fmt.Sprintf("edit_file write error: %v", err), true, "", ""
		}
		return fmt.Sprintf("edited %s", strArg("path")), false, textdiff.Unified(strArg("path"), string(raw), updated), strArg("path")

	case "list_dir":
		rel := strArg("path")
		if rel == "" {
			rel = "."
		}
		target, err := resolveSafeWorkdirPath(opts.Workdir, rel)
		if err != nil {
			return fmt.Sprintf("list_dir error: %v", err), true, "", ""
		}
		entries, err := os.ReadDir(target)
		if err != nil {
			return fmt.Sprintf("list_dir error: %v", err), true, "", ""
		}
		var names []string
		for _, e := range entries {
			name := e.Name()
			if e.IsDir() {
				name += "/"
			}
			names = append(names, name)
		}
		return strings.Join(names, "\n"), false, "", ""

	case "run_command":
		cmdStr := strings.TrimSpace(strArg("command"))
		if cmdStr == "" {
			return "run_command error: empty command", true, "", ""
		}
		cmdCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		c := exec.CommandContext(cmdCtx, "sh", "-c", cmdStr)
		// Without WaitDelay a killed sh leaves grandchildren (curl, go test)
		// holding the output pipe and CombinedOutput blocks until they exit.
		c.WaitDelay = 2 * time.Second
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
			return fmt.Sprintf("command exited with error (%v):\n%s", err, outStr), true, "", ""
		}
		if strings.TrimSpace(outStr) == "" {
			return "(exit 0, no output)", false, "", ""
		}
		return outStr, false, "", ""

	case "search_files":
		pat := strArg("pattern")
		if strings.TrimSpace(pat) == "" {
			return "search_files error: empty pattern", true, "", ""
		}
		re, err := regexp.Compile(pat)
		if err != nil {
			return fmt.Sprintf("search_files error: invalid regexp: %v", err), true, "", ""
		}
		rel := strArg("path")
		if rel == "" {
			rel = "."
		}
		root, err := resolveSafeWorkdirPath(opts.Workdir, rel)
		if err != nil {
			return fmt.Sprintf("search_files error: %v", err), true, "", ""
		}
		var nameRE *regexp.Regexp
		if g := strings.TrimSpace(strArg("glob")); g != "" {
			if nameRE, err = globToPathRegexp(g); err != nil {
				return fmt.Sprintf("search_files error: bad glob: %v", err), true, "", ""
			}
		}
		maxRes := intArg(args, "max_results", 100)
		if maxRes <= 0 || maxRes > 500 {
			maxRes = 100
		}
		base, _ := filepath.Abs(opts.Workdir)
		if base == "" {
			base, _ = os.Getwd()
		}
		var lines []string
		filesScanned := 0
		truncated := false
		walkErr := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if d.IsDir() {
				if p != root && nebiusSkipDirs[d.Name()] {
					return filepath.SkipDir
				}
				return nil
			}
			relPath, _ := filepath.Rel(base, p)
			relPath = filepath.ToSlash(relPath)
			if nameRE != nil && !nameRE.MatchString(relPath) && !nameRE.MatchString(d.Name()) {
				return nil
			}
			info, ierr := d.Info()
			if ierr != nil || info.Size() > 2*1024*1024 {
				return nil
			}
			data, rerr := os.ReadFile(p)
			if rerr != nil {
				return nil
			}
			if len(data) > 0 && strings.IndexByte(string(data[:min(len(data), 1024)]), 0) >= 0 {
				return nil // binary
			}
			filesScanned++
			for i, ln := range strings.Split(string(data), "\n") {
				if re.MatchString(ln) {
					if len(ln) > 240 {
						ln = ln[:240] + "..."
					}
					lines = append(lines, fmt.Sprintf("%s:%d: %s", relPath, i+1, ln))
					if len(lines) >= maxRes {
						truncated = true
						return errors.New("limit")
					}
				}
			}
			return nil
		})
		if walkErr != nil && walkErr.Error() != "limit" && ctx.Err() != nil {
			return "search_files error: cancelled", true, "", ""
		}
		if len(lines) == 0 {
			return fmt.Sprintf("no matches for /%s/ in %d files", pat, filesScanned), false, "", ""
		}
		res := strings.Join(lines, "\n")
		if truncated {
			res += fmt.Sprintf("\n... [stopped at %d matches; narrow the pattern or path]", maxRes)
		}
		return res, false, "", ""

	case "glob":
		pat := strings.TrimSpace(strArg("pattern"))
		if pat == "" {
			return "glob error: empty pattern", true, "", ""
		}
		if filepath.IsAbs(pat) || strings.HasPrefix(pat, "..") {
			return "glob error: pattern must be relative to the workspace", true, "", ""
		}
		re, err := globToPathRegexp(pat)
		if err != nil {
			return fmt.Sprintf("glob error: %v", err), true, "", ""
		}
		maxRes := intArg(args, "max_results", 200)
		if maxRes <= 0 || maxRes > 2000 {
			maxRes = 200
		}
		base, _ := filepath.Abs(opts.Workdir)
		if base == "" {
			base, _ = os.Getwd()
		}
		type hit struct {
			path string
			mod  time.Time
		}
		var hits []hit
		_ = filepath.WalkDir(base, func(p string, d os.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if d.IsDir() {
				if p != base && nebiusSkipDirs[d.Name()] {
					return filepath.SkipDir
				}
				return nil
			}
			relPath, _ := filepath.Rel(base, p)
			relPath = filepath.ToSlash(relPath)
			if re.MatchString(relPath) {
				info, _ := d.Info()
				var mod time.Time
				if info != nil {
					mod = info.ModTime()
				}
				hits = append(hits, hit{relPath, mod})
			}
			return nil
		})
		if len(hits) == 0 {
			return fmt.Sprintf("no files match %s", pat), false, "", ""
		}
		sort.Slice(hits, func(i, j int) bool { return hits[i].mod.After(hits[j].mod) })
		var names []string
		for i, h := range hits {
			if i >= maxRes {
				names = append(names, fmt.Sprintf("... [%d more]", len(hits)-maxRes))
				break
			}
			names = append(names, h.path)
		}
		return strings.Join(names, "\n"), false, "", ""

	case "tavily_extract":
		var urls []string
		if v, ok := args["urls"].([]any); ok {
			for _, u := range v {
				if s, ok := u.(string); ok && strings.TrimSpace(s) != "" {
					urls = append(urls, strings.TrimSpace(s))
				}
			}
		} else if u := strings.TrimSpace(strArg("url")); u != "" {
			urls = []string{u}
		}
		if len(urls) == 0 {
			return "tavily_extract error: no urls given", true, "", ""
		}
		if len(urls) > 5 {
			urls = urls[:5]
		}
		tvClient := &tavily.Client{
			APIKey:  resolveEnvKey(opts.Env, "TAVILY_API_KEY"),
			BaseURL: resolveEnvKey(opts.Env, "TAVILY_BASE_URL"),
		}
		er, err := tvClient.Extract(ctx, urls)
		if err != nil {
			return fmt.Sprintf("tavily_extract error: %v", err), true, "", ""
		}
		md := tavily.FormatExtractMarkdown(er, 12000)
		if md == "" {
			return "tavily_extract: no content returned", true, "", ""
		}
		return md, false, "", ""

	case "tavily_search":
		q := strings.TrimSpace(strArg("query"))
		if q == "" {
			return "tavily_search error: empty query", true, "", ""
		}
		tvClient := &tavily.Client{
			APIKey:  resolveEnvKey(opts.Env, "TAVILY_API_KEY"),
			BaseURL: resolveEnvKey(opts.Env, "TAVILY_BASE_URL"),
		}
		sr, err := tvClient.Search(ctx, q, 5)
		if err != nil {
			return fmt.Sprintf("tavily_search error: %v", err), true, "", ""
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
		return md, false, "", ""

	default:
		return fmt.Sprintf("unknown tool: %s", call.Function.Name), true, "", ""
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
