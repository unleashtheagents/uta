package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/mattn/go-isatty"

	"github.com/unleashtheagents/uta/internal/policy"
	"github.com/unleashtheagents/uta/internal/provider"
	"github.com/unleashtheagents/uta/internal/store"
)

// toolGate is the CLI's provider.ToolGate: it applies the merged
// permissions policy, asks the user when a rule says so (one prompt at a
// time, even with parallel workers), runs pre/post hooks, and snapshots
// files before write_file / edit_file so a run can be undone.
type toolGate struct {
	policy      *policy.Policy
	preApprove  map[string]bool
	interactive bool
	stdin       *bufio.Reader
	stdinFile   *os.File // non-nil when stdin is a real terminal
	errw        io.Writer
	workdir     string
	home        string
	store       *store.Store
	blobs       *store.Blobs

	mu sync.Mutex
	// sessionAllow remembers "yes for this run" answers per tool pattern.
	sessionAllow map[string]bool
}

// newToolGate builds the gate for one run. stdin/errw are the terminal the
// prompt uses; when stdin is not a terminal, "ask" rules deny with a hint.
func newToolGate(app *App, workdir string, preApprove []string, stdin io.Reader, errw io.Writer) (*toolGate, error) {
	if strings.TrimSpace(workdir) == "" {
		if cwd, err := os.Getwd(); err == nil {
			workdir = cwd
		}
	}
	pol, err := policy.LoadMerged(app.GlobalHome, workdir)
	if err != nil {
		return nil, err
	}
	g := &toolGate{
		policy:       pol,
		preApprove:   map[string]bool{},
		stdin:        bufio.NewReader(stdin),
		errw:         errw,
		workdir:      workdir,
		home:         app.GlobalHome,
		store:        app.Store,
		blobs:        app.Blobs,
		sessionAllow: map[string]bool{},
	}
	for _, t := range preApprove {
		g.preApprove[strings.TrimSpace(t)] = true
	}
	if f, ok := stdin.(*os.File); ok && isatty.IsTerminal(f.Fd()) {
		g.interactive = true
		g.stdinFile = f
	}
	if v := os.Getenv("UTA_TOOL_PROMPT"); v == "0" || strings.EqualFold(v, "off") {
		g.interactive = false
	}
	return g, nil
}

func (g *toolGate) preApproved(tool string) bool {
	return g.preApprove["*"] || g.preApprove[tool] || g.preApprove["all"]
}

// Check implements provider.ToolGate.
func (g *toolGate) Check(ctx context.Context, req provider.ToolRequest) (provider.ToolDecision, error) {
	decision := g.decide(ctx, req)
	if !decision.Allow {
		return decision, nil
	}
	if block, warnings := g.policy.RunPreHooks(ctx, g.hookEnv(req, "", false)); block != nil || len(warnings) > 0 {
		for _, w := range warnings {
			fmt.Fprintf(g.errw, "[uta] pre_tool hook %q exited %d: %s\n", w.Hook.Run, w.Exit, firstLine(w.Output))
		}
		if block != nil {
			reason := strings.TrimSpace(block.Output)
			if reason == "" {
				reason = "blocked by pre_tool hook " + block.Hook.Run
			}
			return provider.ToolDecision{Allow: false, Reason: reason, Source: "hook"}, nil
		}
	}
	if req.Tool == "write_file" || req.Tool == "edit_file" {
		if err := g.checkpoint(req); err != nil {
			fmt.Fprintf(g.errw, "[uta] warn: file checkpoint for %s failed: %v\n", req.Summary, err)
		}
	}
	return decision, nil
}

func (g *toolGate) decide(ctx context.Context, req provider.ToolRequest) provider.ToolDecision {
	args := json.RawMessage(req.Arguments)
	if !json.Valid(args) {
		args = json.RawMessage("{}")
	}
	verdict, rule := g.policy.Decide(req.Tool, args)
	switch verdict {
	case policy.Deny:
		return provider.ToolDecision{Allow: false, Reason: "matches deny rule " + rule, Source: "policy"}
	case policy.Allow:
		if rule == "default" {
			return provider.ToolDecision{Allow: true, Reason: "read-only tool", Source: "default"}
		}
		return provider.ToolDecision{Allow: true, Reason: "allow rule " + rule, Source: "policy"}
	}
	// Ask.
	if g.preApproved(req.Tool) {
		return provider.ToolDecision{Allow: true, Reason: "--pre-approve", Source: "pre-approve"}
	}
	pattern := policy.AlwaysPattern(req.Tool, req.Summary)
	g.mu.Lock()
	sessionOK := g.sessionAllow[req.Tool] || g.sessionAllow[pattern]
	g.mu.Unlock()
	if sessionOK {
		return provider.ToolDecision{Allow: true, Reason: "approved earlier in this run", Source: "prompt"}
	}
	if !g.interactive {
		hint := fmt.Sprintf("needs approval (rule: %s); run in a terminal to be asked, pass --pre-approve %s, or add an allow rule with: uta permissions allow '%s'", rule, req.Tool, pattern)
		return provider.ToolDecision{Allow: false, Reason: hint, Source: "policy"}
	}
	return g.prompt(ctx, req, pattern)
}

// prompt asks the user. Answers: y (once), s (this run, same tool),
// a (always: write an allow rule), n (deny).
func (g *toolGate) prompt(ctx context.Context, req provider.ToolRequest, pattern string) provider.ToolDecision {
	g.mu.Lock()
	defer g.mu.Unlock()
	// Re-check: another worker may have answered "s"/"a" while we waited.
	if g.sessionAllow[req.Tool] || g.sessionAllow[pattern] {
		return provider.ToolDecision{Allow: true, Reason: "approved earlier in this run", Source: "prompt"}
	}
	summary := req.Summary
	if len(summary) > 200 {
		summary = summary[:200] + "..."
	}
	who := "agent"
	if req.SubtaskID != "" {
		who = "worker " + shortID(req.SubtaskID)
	}
	fmt.Fprintf(g.errw, "\n\x1b[1m[uta] %s wants %s:\x1b[0m %s\n", who, req.Tool, summary)
	fmt.Fprintf(g.errw, "  [y] once  [s] this run  [a] always (%s)  [n] deny  > ", pattern)
	if g.stdinFile != nil {
		resuppress := restoreEchoDuring(g.stdinFile)
		defer resuppress()
	}

	type ans struct {
		line string
		err  error
	}
	ch := make(chan ans, 1)
	go func() {
		line, err := g.stdin.ReadString('\n')
		ch <- ans{line, err}
	}()
	var line string
	select {
	case <-ctx.Done():
		fmt.Fprintln(g.errw)
		return provider.ToolDecision{Allow: false, Reason: "cancelled", Source: "prompt"}
	case a := <-ch:
		if a.err != nil && a.line == "" {
			return provider.ToolDecision{Allow: false, Reason: "no answer (stdin closed)", Source: "prompt"}
		}
		line = strings.ToLower(strings.TrimSpace(a.line))
	}
	switch line {
	case "y", "yes":
		return provider.ToolDecision{Allow: true, Reason: "approved once", Source: "prompt"}
	case "s", "session", "run":
		g.sessionAllow[req.Tool] = true
		return provider.ToolDecision{Allow: true, Reason: "approved for this run", Source: "prompt"}
	case "a", "always":
		g.sessionAllow[pattern] = true
		target := g.rulesFile()
		if err := policy.AddRule(target, policy.Allow, pattern); err != nil {
			fmt.Fprintf(g.errw, "[uta] could not save rule: %v\n", err)
		} else {
			fmt.Fprintf(g.errw, "[uta] saved allow rule %s to %s\n", pattern, target)
			if p, err := policy.LoadMerged(g.home, g.workdir); err == nil {
				g.policy = p
			}
		}
		return provider.ToolDecision{Allow: true, Reason: "approved, rule saved", Source: "prompt"}
	}
	return provider.ToolDecision{Allow: false, Reason: "denied by user at prompt", Source: "prompt"}
}

// rulesFile picks where "always" rules go: the project file when the
// workdir already has a .uta directory, the global file otherwise.
func (g *toolGate) rulesFile() string {
	if st, err := os.Stat(filepath.Join(g.workdir, ".uta")); err == nil && st.IsDir() {
		return policy.ProjectFile(g.workdir)
	}
	return policy.GlobalFile(g.home)
}

// After implements provider.ToolGate: post hooks whose exit status is 2
// feed their output back to the model.
func (g *toolGate) After(ctx context.Context, req provider.ToolRequest, output string, isErr bool) string {
	feedback, warnings := g.policy.RunPostHooks(ctx, g.hookEnv(req, output, isErr))
	for _, w := range warnings {
		fmt.Fprintf(g.errw, "[uta] post_tool hook %q exited %d: %s\n", w.Hook.Run, w.Exit, firstLine(w.Output))
	}
	var parts []string
	for _, f := range feedback {
		if strings.TrimSpace(f.Output) != "" {
			parts = append(parts, "[hook feedback] "+strings.TrimSpace(f.Output))
		}
	}
	return strings.Join(parts, "\n")
}

func (g *toolGate) hookEnv(req provider.ToolRequest, output string, isErr bool) policy.HookEnv {
	return policy.HookEnv{
		Tool: req.Tool, Args: req.Arguments, Summary: req.Summary,
		Session: req.SessionID, Workdir: g.workdir, Output: output, IsErr: isErr,
	}
}

// checkpoint stores the file's current content before the tool changes it.
func (g *toolGate) checkpoint(req provider.ToolRequest) error {
	if g.store == nil || g.blobs == nil || req.SessionID == "" {
		return nil
	}
	var args struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal([]byte(req.Arguments), &args); err != nil || strings.TrimSpace(args.Path) == "" {
		return nil
	}
	rel := filepath.Clean(args.Path)
	if filepath.IsAbs(rel) || strings.HasPrefix(rel, "..") {
		return nil // the tool itself will refuse it
	}
	cp := store.FileCheckpoint{SessionID: req.SessionID, SubtaskID: req.SubtaskID, Tool: req.Tool, Path: filepath.ToSlash(rel), Workdir: g.workdir}
	data, err := os.ReadFile(filepath.Join(g.workdir, rel))
	switch {
	case err == nil:
		ref, perr := g.blobs.Put(data, "checkpoint")
		if perr != nil {
			return perr
		}
		cp.Existed = true
		cp.BeforeRef = ref
	case os.IsNotExist(err):
		cp.Existed = false
	default:
		return err
	}
	_, err = g.store.InsertFileCheckpoint(cp)
	return err
}
