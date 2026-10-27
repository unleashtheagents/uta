// Package policy is the permission model for tools uta runs itself (the
// nebius provider's native tools). It answers one question per tool call:
// allow, ask or deny, and runs the user's pre/post tool hooks.
//
// Rules live in two YAML files that are merged, project over global:
//
//	~/.uta/permissions.yaml            applies everywhere
//	<workdir>/.uta/permissions.yaml    applies to one repository
//
// Pattern grammar is the one uta already uses for mission profiles and
// claude's --allowedTools: `tool` matches every call of a tool, `tool(glob)`
// matches when the call's command, path or query matches the glob (* and ?).
// Deny wins over ask, ask wins over allow. Calls no rule matches fall back
// to the defaults: read-only tools run, anything that writes or executes
// asks.
package policy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/unleashtheagents/uta/internal/engine/capability"
)

// Verdict is what the rules say about a call.
type Verdict int

const (
	Ask Verdict = iota
	Allow
	Deny
)

func (v Verdict) String() string {
	switch v {
	case Allow:
		return "allow"
	case Deny:
		return "deny"
	}
	return "ask"
}

// Hook is one shell command run around a tool call.
type Hook struct {
	// Match is a rule pattern (same grammar as allow/ask/deny); empty
	// matches every tool.
	Match string `yaml:"match,omitempty"`
	// Run is passed to `sh -c` with UTA_TOOL, UTA_TOOL_ARGS, UTA_TOOL_PATH,
	// UTA_TOOL_COMMAND, UTA_SESSION and UTA_WORKDIR set. Post hooks also get
	// UTA_TOOL_OUTPUT and UTA_TOOL_ERROR.
	Run string `yaml:"run"`
	// Timeout caps the hook; 0 means 60s.
	Timeout time.Duration `yaml:"timeout,omitempty"`
}

// Hooks groups the two hook points.
type Hooks struct {
	// PreTool runs before the tool. Exit status 2 denies the call and the
	// hook's output is shown to the model as the reason; other non-zero
	// exits are reported and ignored.
	PreTool []Hook `yaml:"pre_tool,omitempty"`
	// PostTool runs after the tool. Exit status 2 appends the hook's output
	// to what the model sees (use it to push lint or test feedback back
	// into the loop).
	PostTool []Hook `yaml:"post_tool,omitempty"`
}

// File is the on-disk shape of permissions.yaml.
type File struct {
	Allow []string `yaml:"allow,omitempty"`
	Ask   []string `yaml:"ask,omitempty"`
	Deny  []string `yaml:"deny,omitempty"`
	Hooks Hooks    `yaml:"hooks,omitempty"`
}

// Policy is the merged, parsed rule set.
type Policy struct {
	Allow, Ask, Deny []string
	Hooks            Hooks
	// Sources lists the files that contributed rules, for `uta permissions`.
	Sources []string

	allow, ask, deny *capability.Gate
}

// ReadOnlyTools run without asking when no rule says otherwise.
var ReadOnlyTools = map[string]bool{
	"read_file": true, "list_dir": true, "search_files": true, "glob": true,
	"tavily_search": true, "tavily_extract": true,
}

// GlobalFile is the user-wide rules file under home.
func GlobalFile(home string) string { return filepath.Join(home, "permissions.yaml") }

// ProjectFile is the per-repository rules file under workdir.
func ProjectFile(workdir string) string { return filepath.Join(workdir, ".uta", "permissions.yaml") }

// Load reads a single rules file. A missing file yields an empty File.
func Load(path string) (File, error) {
	var f File
	raw, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return f, nil
		}
		return f, err
	}
	if err := yaml.Unmarshal(raw, &f); err != nil {
		return f, fmt.Errorf("%s: %w", path, err)
	}
	return f, nil
}

// Save writes a rules file, creating its directory.
func Save(path string, f File) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	out, err := yaml.Marshal(f)
	if err != nil {
		return err
	}
	header := "# uta tool permissions. Patterns: tool  or  tool(glob). deny > ask > allow.\n# Tools: read_file list_dir search_files glob tavily_search tavily_extract write_file edit_file run_command\n"
	return os.WriteFile(path, append([]byte(header), out...), 0o644)
}

// LoadMerged builds the effective policy for a workdir: the global file
// plus the project file. Lists are unioned; project hooks run after
// global hooks.
func LoadMerged(home, workdir string) (*Policy, error) {
	p := &Policy{}
	paths := []string{}
	if strings.TrimSpace(home) != "" {
		paths = append(paths, GlobalFile(home))
	}
	if strings.TrimSpace(workdir) != "" {
		paths = append(paths, ProjectFile(workdir))
	}
	for _, path := range paths {
		f, err := Load(path)
		if err != nil {
			return nil, err
		}
		if _, statErr := os.Stat(path); statErr == nil {
			p.Sources = append(p.Sources, path)
		}
		p.Allow = append(p.Allow, f.Allow...)
		p.Ask = append(p.Ask, f.Ask...)
		p.Deny = append(p.Deny, f.Deny...)
		p.Hooks.PreTool = append(p.Hooks.PreTool, f.Hooks.PreTool...)
		p.Hooks.PostTool = append(p.Hooks.PostTool, f.Hooks.PostTool...)
	}
	p.compile()
	return p, nil
}

// New builds a policy from in-memory lists (tests, embedded defaults).
func New(allow, ask, deny []string) *Policy {
	p := &Policy{Allow: allow, Ask: ask, Deny: deny}
	p.compile()
	return p
}

func (p *Policy) compile() {
	p.allow = capability.NewGate(nil, p.Allow)
	p.ask = capability.NewGate(nil, p.Ask)
	p.deny = capability.NewGate(nil, p.Deny)
}

// matches reports whether any pattern in g matches the call. The gate is
// built with the patterns as its deny list, so Deny means "matched".
func matches(g *capability.Gate, tool string, args json.RawMessage) (bool, string) {
	if g == nil || g.Empty() {
		return false, ""
	}
	d, pat := g.Inspect(tool, args)
	return d == capability.Deny, pat
}

// Decide evaluates one call. rule is the pattern that decided it, or
// "default" when the built-in fallback applied.
func (p *Policy) Decide(tool string, args json.RawMessage) (v Verdict, rule string) {
	if p == nil {
		return defaultVerdict(tool), "default"
	}
	if ok, pat := matches(p.deny, tool, args); ok {
		return Deny, pat
	}
	if ok, pat := matches(p.ask, tool, args); ok {
		return Ask, pat
	}
	if ok, pat := matches(p.allow, tool, args); ok {
		return Allow, pat
	}
	return defaultVerdict(tool), "default"
}

func defaultVerdict(tool string) Verdict {
	if ReadOnlyTools[tool] {
		return Allow
	}
	return Ask
}

// AlwaysPattern is the rule written when a user answers "always" at the
// prompt: file tools are allowed outright, commands are allowed by their
// first two words (`go test ./...` becomes `run_command(go test *)`).
func AlwaysPattern(tool, summary string) string {
	if tool != "run_command" {
		return tool
	}
	fields := strings.Fields(summary)
	switch len(fields) {
	case 0:
		return tool
	case 1:
		return fmt.Sprintf("%s(%s *)", tool, fields[0])
	}
	return fmt.Sprintf("%s(%s %s *)", tool, fields[0], fields[1])
}

// AddRule appends pattern to the named list in the rules file at path,
// skipping duplicates.
func AddRule(path string, list Verdict, pattern string) error {
	f, err := Load(path)
	if err != nil {
		return err
	}
	pattern = strings.TrimSpace(pattern)
	if pattern == "" {
		return errors.New("empty pattern")
	}
	target := &f.Allow
	switch list {
	case Ask:
		target = &f.Ask
	case Deny:
		target = &f.Deny
	}
	for _, existing := range *target {
		if existing == pattern {
			return nil
		}
	}
	*target = append(*target, pattern)
	return Save(path, f)
}

// HookEnv is what hooks learn about the call.
type HookEnv struct {
	Tool, Args, Summary, Session, Workdir string
	Output                                string
	IsErr                                 bool
}

// HookResult is one hook's outcome.
type HookResult struct {
	Hook   Hook
	Exit   int
	Output string
	Err    error
}

// RunPreHooks runs matching pre-tool hooks in order. It returns the first
// result whose exit status is 2 (a block) or nil when none blocked; other
// failures are collected in warnings.
func (p *Policy) RunPreHooks(ctx context.Context, env HookEnv) (block *HookResult, warnings []HookResult) {
	if p == nil {
		return nil, nil
	}
	for _, h := range p.Hooks.PreTool {
		if !hookMatches(h, env.Tool, env.Args) {
			continue
		}
		r := runHook(ctx, h, env)
		if r.Exit == 2 {
			return &r, warnings
		}
		if r.Exit != 0 || r.Err != nil {
			warnings = append(warnings, r)
		}
	}
	return nil, warnings
}

// RunPostHooks runs matching post-tool hooks and returns the outputs of
// those that exited 2 (feedback for the model) plus other failures.
func (p *Policy) RunPostHooks(ctx context.Context, env HookEnv) (feedback []HookResult, warnings []HookResult) {
	if p == nil {
		return nil, nil
	}
	for _, h := range p.Hooks.PostTool {
		if !hookMatches(h, env.Tool, env.Args) {
			continue
		}
		r := runHook(ctx, h, env)
		switch {
		case r.Exit == 2:
			feedback = append(feedback, r)
		case r.Exit != 0 || r.Err != nil:
			warnings = append(warnings, r)
		}
	}
	return feedback, warnings
}

func hookMatches(h Hook, tool string, args string) bool {
	if strings.TrimSpace(h.Match) == "" {
		return true
	}
	ok, _ := matches(capability.NewGate(nil, []string{h.Match}), tool, json.RawMessage(args))
	return ok
}

func runHook(ctx context.Context, h Hook, env HookEnv) HookResult {
	timeout := h.Timeout
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	hctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	c := exec.CommandContext(hctx, "sh", "-c", h.Run)
	if env.Workdir != "" {
		c.Dir = env.Workdir
	}
	isErr := "0"
	if env.IsErr {
		isErr = "1"
	}
	out := env.Output
	if len(out) > 32*1024 {
		out = out[:32*1024]
	}
	var path, command string
	var argMap map[string]any
	if json.Unmarshal([]byte(env.Args), &argMap) == nil {
		if v, ok := argMap["path"].(string); ok {
			path = v
		}
		if v, ok := argMap["command"].(string); ok {
			command = v
		}
	}
	c.Env = append(os.Environ(),
		"UTA_TOOL="+env.Tool,
		"UTA_TOOL_ARGS="+env.Args,
		"UTA_TOOL_SUMMARY="+env.Summary,
		"UTA_TOOL_PATH="+path,
		"UTA_TOOL_COMMAND="+command,
		"UTA_SESSION="+env.Session,
		"UTA_WORKDIR="+env.Workdir,
		"UTA_TOOL_OUTPUT="+out,
		"UTA_TOOL_ERROR="+isErr,
	)
	b, err := c.CombinedOutput()
	r := HookResult{Hook: h, Output: strings.TrimSpace(string(b))}
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			r.Exit = ee.ExitCode()
		} else {
			r.Err = err
			r.Exit = -1
		}
	}
	return r
}

// Template is a commented starter file for `uta permissions init`.
const Template = `# uta tool permissions for this repository.
# Patterns: tool            every call of the tool
#           tool(glob)      calls whose command/path/query matches (* and ?)
# deny beats ask, ask beats allow. Unmatched read-only tools run; unmatched
# write_file / edit_file / run_command ask (or are denied without a terminal).
allow:
  - run_command(go test *)
  - run_command(go build *)
  - run_command(go vet *)
  - run_command(git status*)
  - run_command(git diff*)
  - write_file
  - edit_file
ask:
  - run_command(git push *)
deny:
  - run_command(* rm -rf *)
  - run_command(*| sh*)
  - run_command(*| bash*)
  - run_command(*sudo *)
  - write_file(.env*)
  - edit_file(.env*)
hooks:
  pre_tool: []
  post_tool:
    # Exit 2 sends the hook's output back to the model as feedback.
    # - match: edit_file(*.go)
    #   run: gofmt -l "$UTA_TOOL_PATH" | grep . && { echo "gofmt wants changes in $UTA_TOOL_PATH"; exit 2; } || true
`
