package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/unleashtheagents/uta/internal/engine"
	"github.com/unleashtheagents/uta/internal/hitl"
	"github.com/unleashtheagents/uta/internal/memory"
	"github.com/unleashtheagents/uta/internal/profile"
	"github.com/unleashtheagents/uta/internal/state"
	"github.com/unleashtheagents/uta/internal/trajectory"
	"github.com/unleashtheagents/uta/internal/whiteboard"
)

// shellBackend is what the REPL loop needs from the world. The production
// implementation (utaShell) wires it to the engine, store, and registry;
// tests inject a fake to exercise parsing and dispatch without providers.
type shellBackend interface {
	// ExecTurn sends a goal to the active worker. The first turn starts a
	// fresh orchestration run; subsequent turns resume the attached session
	// so the conversation keeps its context.
	ExecTurn(ctx context.Context, goal string) error
	// ExecTurnAs routes one turn to a specific worker without changing the
	// shell's active worker ("@gemini cross-check this").
	ExecTurnAs(ctx context.Context, worker, goal string) error
	// SessionShort returns the short id of the attached session, or ""
	// when the next input will start a new run.
	SessionShort() string
	NewConversation()
	// Attach resolves a (possibly short) session id and makes it the
	// conversation the next turn resumes.
	Attach(id string) (string, error)
	Sessions(w io.Writer, limit int) error
	Agents(ctx context.Context, w io.Writer) error
	Worker() string
	SetWorker(ctx context.Context, name string) error
	ModeName() string
	SetMode(name string) error
	ClearMode()
	// Model reports the model pin for the active worker, "" when the
	// provider default applies. SetModel accepts a bare model name (mapped
	// to the provider's env var) or an explicit VAR=value spec.
	Model() string
	SetModel(spec string) error
	ClearModel()
	Tools() []string
	SetTools(tools []string)
	Workdir() string
	SetWorkdir(dir string) error
	LocalExec(ctx context.Context, cmdline string) error
	// RunMissionFile interprets a .steer program (see docs/steer.md) with
	// the shell's worker and workdir. checkOnly runs the static checks and
	// spends nothing. Missions are their own sessions — the shell's
	// attached conversation is left untouched.
	RunMissionFile(ctx context.Context, path string, checkOnly bool) error
	Status(w io.Writer)
}

func newShellCmd() *cobra.Command {
	var (
		workerName     string
		workdir        string
		sessionID      string
		preApprove     []string
		maxParallel    int
		maxSubtasks    int
		subtaskTimeout time.Duration
		runTimeout     time.Duration
	)
	cmd := &cobra.Command{
		Use:     "shell",
		Aliases: []string{"repl"},
		Short:   "interactive shell: converse with and control agents in one persistent session",
		Long: `An interactive prompt for driving agents, terminal-style. Type a goal and
it fans out to the active worker; type again and the same conversation
continues (each turn resumes the prior session). Slash commands control the
agent side: switch workers, switch mission profiles, attach to old sessions,
inspect providers. Lines starting with '!' run locally in the workdir.`,
		Example: `  # start the shell, picking the first detected provider
  uta shell

  # pin the worker and pre-approve tools for every turn
  uta shell --worker claude --pre-approve Read,Bash

  # re-enter an old conversation
  uta shell --session 1a2b3c4d`,
		RunE: func(cmd *cobra.Command, args []string) error {
			app, err := newApp(cmd.Context())
			if err != nil {
				return err
			}
			defer app.Close()

			detections := app.Registry.DetectAll(cmd.Context())
			available := availableProviders(app.Registry.Names(), detections)
			if len(available) == 0 {
				fmt.Fprintln(cmd.ErrOrStderr(), errNoProviders)
				return exitWith(3)
			}
			if workerName == "" {
				workerName = available[0]
			} else if !containsString(available, workerName) {
				det := detections[workerName]
				return fmt.Errorf("provider %q is not available: %s", workerName, firstNonEmptyStr(det.Notes, "not detected"))
			}

			mode, err := resolveActiveMode(cmd, app)
			if err != nil {
				return err
			}

			if app.InProject() && workdir == "" {
				workdir = app.ProjectRoot
			}

			noColor, _ := cmd.Flags().GetBool("no-color")
			sh := &utaShell{
				app:            app,
				out:            cmd.OutOrStdout(),
				errw:           cmd.ErrOrStderr(),
				stdin:          cmd.InOrStdin(),
				noColor:        noColor,
				worker:         workerName,
				workdir:        workdir,
				mode:           mode,
				models:         map[string]string{},
				preApprove:     preApprove,
				maxParallel:    maxParallel,
				maxSubtasks:    maxSubtasks,
				subtaskTimeout: subtaskTimeout,
				runTimeout:     runTimeout,
			}
			if sessionID != "" {
				resolved, aerr := sh.Attach(sessionID)
				if aerr != nil {
					return aerr
				}
				fmt.Fprintf(cmd.ErrOrStderr(), "[uta] attached to session %s\n", shortID(resolved))
			}

			modeName := "(none)"
			if mode != nil {
				modeName = mode.Name
			}
			fmt.Fprintln(cmd.ErrOrStderr(), "[uta] shell — text talks to agents, /commands control them, !commands run locally. /help for details, /exit to leave.")
			fmt.Fprintf(cmd.ErrOrStderr(), "[uta] worker: %s   mode: %s   agents detected: %s\n",
				workerName, modeName, strings.Join(available, ", "))
			return shellLoop(cmd.Context(), cmd.InOrStdin(), cmd.OutOrStdout(), cmd.ErrOrStderr(), sh)
		},
	}
	cmd.Flags().StringVar(&workerName, "worker", "", "worker provider (defaults to the first detected one)")
	cmd.Flags().StringVar(&workdir, "workdir", "", "working directory exposed to the worker (defaults to CWD / project root)")
	cmd.Flags().StringVar(&sessionID, "session", "", "attach to an existing session at startup (short ids work)")
	cmd.Flags().StringSliceVar(&preApprove, "pre-approve", nil, "tools the worker may use without prompting, applied to every turn")
	cmd.Flags().IntVar(&maxParallel, "max-parallel", 4, "maximum concurrent subtasks per turn")
	cmd.Flags().IntVar(&maxSubtasks, "max-subtasks", 6, "hard cap on subtasks the planner may propose per turn")
	cmd.Flags().DurationVar(&subtaskTimeout, "subtask-timeout", 10*time.Minute, "per-subtask timeout")
	cmd.Flags().DurationVar(&runTimeout, "timeout", 30*time.Minute, "per-turn timeout")
	return cmd
}

// shellLoop is the read-dispatch cycle. It owns nothing but the scanner;
// every effect goes through the backend, so tests drive it with a
// strings.Reader and a fake. EOF (Ctrl-D) exits cleanly.
func shellLoop(ctx context.Context, in io.Reader, out, errw io.Writer, b shellBackend) error {
	sc := bufio.NewScanner(in)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		fmt.Fprint(out, shellPrompt(b))
		if !sc.Scan() {
			fmt.Fprintln(out)
			return sc.Err()
		}
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		if dispatchShellLine(ctx, line, b, out, errw) {
			return nil
		}
	}
}

func shellPrompt(b shellBackend) string {
	if s := b.SessionShort(); s != "" {
		return fmt.Sprintf("uta [%s]> ", s)
	}
	return "uta> "
}

// dispatchShellLine routes one input line: bare exit words and slash
// commands are shell-side, '!' runs locally, anything else is a goal for
// the agents. Returns true when the shell should exit. Errors are printed,
// never fatal — a failed turn must not tear down the session.
func dispatchShellLine(ctx context.Context, line string, b shellBackend, out, errw io.Writer) bool {
	switch {
	case line == "exit" || line == "quit" || line == "/exit" || line == "/quit":
		return true
	case strings.HasPrefix(line, "!"):
		cmdline := strings.TrimSpace(line[1:])
		if cmdline == "" {
			fmt.Fprintln(errw, "usage: !<command>")
			return false
		}
		if err := b.LocalExec(ctx, cmdline); err != nil {
			fmt.Fprintln(errw, "!:", err)
		}
		return false
	case strings.HasPrefix(line, "@"):
		rest := strings.TrimSpace(line[1:])
		agent, goal, _ := strings.Cut(rest, " ")
		goal = strings.TrimSpace(goal)
		if agent == "" || goal == "" {
			fmt.Fprintln(errw, "usage: @<agent> <goal>   (e.g. @gemini cross-check the last answer)")
			return false
		}
		if err := b.ExecTurnAs(ctx, agent, goal); err != nil {
			fmt.Fprintln(errw, "turn failed:", err)
		}
		return false
	case strings.HasPrefix(line, "/"):
		handleShellCommand(ctx, line, b, out, errw)
		return false
	default:
		if err := b.ExecTurn(ctx, line); err != nil {
			fmt.Fprintln(errw, "turn failed:", err)
		}
		return false
	}
}

func handleShellCommand(ctx context.Context, line string, b shellBackend, out, errw io.Writer) {
	fields := strings.Fields(line)
	name := strings.TrimPrefix(fields[0], "/")
	args := fields[1:]
	switch name {
	case "help":
		printShellHelp(out)
	case "new", "clear":
		b.NewConversation()
		fmt.Fprintln(out, "new conversation — next input starts a fresh run.")
	case "use", "resume":
		if len(args) != 1 {
			fmt.Fprintf(errw, "usage: %s <session-id>\n", fields[0])
			return
		}
		resolved, err := b.Attach(args[0])
		if err != nil {
			fmt.Fprintln(errw, "use:", err)
			return
		}
		fmt.Fprintf(out, "attached to session %s — next input resumes it.\n", shortID(resolved))
	case "sessions":
		limit := 10
		if len(args) > 0 {
			n, err := strconv.Atoi(args[0])
			if err != nil || n < 1 {
				fmt.Fprintln(errw, "usage: /sessions [n]")
				return
			}
			limit = n
		}
		if err := b.Sessions(out, limit); err != nil {
			fmt.Fprintln(errw, "sessions:", err)
		}
	case "agents", "providers":
		if err := b.Agents(ctx, out); err != nil {
			fmt.Fprintln(errw, "agents:", err)
		}
	case "worker":
		if len(args) == 0 {
			fmt.Fprintf(out, "worker: %s\n", b.Worker())
			return
		}
		if err := b.SetWorker(ctx, args[0]); err != nil {
			fmt.Fprintln(errw, "worker:", err)
			return
		}
		fmt.Fprintf(out, "worker: %s\n", b.Worker())
	case "mode":
		if len(args) == 0 {
			if m := b.ModeName(); m != "" {
				fmt.Fprintf(out, "mode: %s\n", m)
			} else {
				fmt.Fprintln(out, "mode: (none)")
			}
			return
		}
		if args[0] == "none" {
			b.ClearMode()
			fmt.Fprintln(out, "mode: (none)")
			return
		}
		if err := b.SetMode(args[0]); err != nil {
			fmt.Fprintln(errw, "mode:", err)
			return
		}
		fmt.Fprintf(out, "mode: %s\n", b.ModeName())
	case "model":
		if len(args) == 0 {
			if m := b.Model(); m != "" {
				fmt.Fprintf(out, "model: %s\n", m)
			} else {
				fmt.Fprintln(out, "model: (provider default)")
			}
			return
		}
		if args[0] == "default" {
			b.ClearModel()
			fmt.Fprintln(out, "model: (provider default)")
			return
		}
		if err := b.SetModel(args[0]); err != nil {
			fmt.Fprintln(errw, "model:", err)
			return
		}
		fmt.Fprintf(out, "model: %s\n", b.Model())
	case "tools":
		if len(args) == 0 {
			if tools := b.Tools(); len(tools) > 0 {
				fmt.Fprintf(out, "pre-approved tools: %s\n", strings.Join(tools, ", "))
			} else {
				fmt.Fprintln(out, "pre-approved tools: (none — the provider prompts per tool)")
			}
			return
		}
		if len(args) == 1 && args[0] == "none" {
			b.SetTools(nil)
			fmt.Fprintln(out, "pre-approved tools: (none)")
			return
		}
		var tools []string
		for _, a := range args {
			for _, t := range strings.Split(a, ",") {
				if t = strings.TrimSpace(t); t != "" {
					tools = append(tools, t)
				}
			}
		}
		b.SetTools(tools)
		fmt.Fprintf(out, "pre-approved tools: %s\n", strings.Join(tools, ", "))
	case "mission":
		checkOnly := false
		margs := args
		if len(margs) > 0 && (margs[0] == "check" || margs[0] == "run") {
			checkOnly = margs[0] == "check"
			margs = margs[1:]
		}
		if len(margs) != 1 {
			fmt.Fprintln(errw, "usage: /mission [check] <file.steer>")
			return
		}
		if err := b.RunMissionFile(ctx, margs[0], checkOnly); err != nil {
			fmt.Fprintln(errw, "mission:", err)
		}
	case "cd":
		if len(args) == 0 {
			fmt.Fprintf(out, "workdir: %s\n", firstNonEmptyStr(b.Workdir(), "(current directory)"))
			return
		}
		if len(args) != 1 {
			fmt.Fprintln(errw, "usage: /cd [dir]")
			return
		}
		if err := b.SetWorkdir(args[0]); err != nil {
			fmt.Fprintln(errw, "cd:", err)
			return
		}
		fmt.Fprintf(out, "workdir: %s\n", b.Workdir())
	case "status":
		b.Status(out)
	default:
		fmt.Fprintf(errw, "unknown command /%s — try /help\n", name)
	}
}

func printShellHelp(w io.Writer) {
	fmt.Fprint(w, `Every line you type is one of four things:

  text    a goal for the agents           @name text    same, routed to one agent
  /cmd    control the shell and agents    !cmd          run locally, no agents

conversation
  <text>              send a goal to the active agent; the first input starts
                      a run, later inputs continue the same conversation
  @<agent> <text>     route one turn to a specific agent (e.g. @gemini ...)
  /new  /clear        start a fresh conversation
  /use <session-id>   attach to an existing session (/resume works too)
  /sessions [n]       list recent sessions (default 10)

agent control
  /agents             list agent providers and availability (* = active worker)
  /worker [name]      show or switch the active worker agent
  /model [m|default]  show or pin the model the active worker runs on
                      (bare name for claude/gemini, VAR=value for others)
  /tools [t,...|none] show or set tools pre-approved for every turn
  /mode [name|none]   show, switch, or clear the active mission profile
  /mission [check] <file.steer>
                      run (or just static-check) a steer program with the
                      shell's worker and workdir — see docs/steer.md
  /status             show worker, session, model, mode, and workdir

terminal
  !<command>          run a local shell command in the workdir
  /cd [dir]           show or change the workdir agents operate in
  /help               this help
  /exit               leave the shell (Ctrl-D and 'exit' work too)
`)
}

// utaShell is the production shellBackend: one App for the whole session,
// a fresh bus + renderer + supervisor per turn (mirroring what `uta run`
// and `uta resume` each do per invocation).
type utaShell struct {
	app     *App
	out     io.Writer
	errw    io.Writer
	stdin   io.Reader
	noColor bool

	worker    string
	sessionID string
	workdir   string
	mode      *profile.MissionProfile
	// models pins a model per worker name, stored as a ready-to-inject
	// "VAR=value" env entry (e.g. "ANTHROPIC_MODEL=claude-fable-5").
	models map[string]string

	preApprove     []string
	maxParallel    int
	maxSubtasks    int
	subtaskTimeout time.Duration
	runTimeout     time.Duration
}

func (s *utaShell) SessionShort() string {
	if s.sessionID == "" {
		return ""
	}
	return shortID(s.sessionID)
}

func (s *utaShell) NewConversation() { s.sessionID = "" }

func (s *utaShell) Attach(id string) (string, error) {
	resolved, err := s.app.Store.ResolveSessionID(strings.TrimSpace(id))
	if err != nil {
		return "", err
	}
	s.sessionID = resolved
	return resolved, nil
}

func (s *utaShell) Worker() string { return s.worker }

func (s *utaShell) SetWorker(ctx context.Context, name string) error {
	detections := s.app.Registry.DetectAll(ctx)
	available := availableProviders(s.app.Registry.Names(), detections)
	if !containsString(available, name) {
		det := detections[name]
		return fmt.Errorf("provider %q is not available: %s", name, firstNonEmptyStr(det.Notes, "not detected"))
	}
	s.worker = name
	return nil
}

func (s *utaShell) ModeName() string {
	if s.mode == nil {
		return ""
	}
	return s.mode.Name
}

func (s *utaShell) SetMode(name string) error {
	p, err := resolveModeByName(s.app, name)
	if err != nil {
		return err
	}
	s.mode = p
	return nil
}

func (s *utaShell) ClearMode() { s.mode = nil }

// modelEnvVarFor maps a worker to the env var its CLI honors for model
// selection (see docs/model-selection.md). Empty for providers whose env
// contract uta doesn't know — /model then needs the explicit VAR=value form.
func modelEnvVarFor(worker string) string {
	switch worker {
	case "claude":
		return "ANTHROPIC_MODEL"
	case "gemini":
		return "GEMINI_MODEL"
	}
	return ""
}

func (s *utaShell) Model() string { return s.models[s.worker] }

func (s *utaShell) SetModel(spec string) error {
	if strings.Contains(spec, "=") {
		s.models[s.worker] = spec
		return nil
	}
	envVar := modelEnvVarFor(s.worker)
	if envVar == "" {
		return fmt.Errorf("no known model env var for provider %q — use /model VAR=value (see docs/model-selection.md)", s.worker)
	}
	s.models[s.worker] = envVar + "=" + spec
	return nil
}

func (s *utaShell) ClearModel() { delete(s.models, s.worker) }

func (s *utaShell) Tools() []string { return s.preApprove }

func (s *utaShell) SetTools(tools []string) { s.preApprove = tools }

func (s *utaShell) Workdir() string { return s.workdir }

func (s *utaShell) SetWorkdir(dir string) error {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return err
	}
	info, err := os.Stat(abs)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("not a directory: %s", abs)
	}
	s.workdir = abs
	return nil
}

func (s *utaShell) Status(w io.Writer) {
	session := "(none — next input starts a new run)"
	if s.sessionID != "" {
		session = shortID(s.sessionID)
	}
	modeName := "(none)"
	if s.mode != nil {
		modeName = s.mode.Name
	}
	model := "(provider default)"
	if m := s.models[s.worker]; m != "" {
		model = m
	}
	tools := "(none — the provider prompts per tool)"
	if len(s.preApprove) > 0 {
		tools = strings.Join(s.preApprove, ", ")
	}
	workdir := s.workdir
	if workdir == "" {
		workdir, _ = os.Getwd()
	}
	fmt.Fprintf(w, "worker:  %s\nsession: %s\nmodel:   %s\ntools:   %s\nmode:    %s\nworkdir: %s\n",
		s.worker, session, model, tools, modeName, workdir)
	if s.app.InProject() {
		fmt.Fprintf(w, "project: %s (%s)\n", s.app.ProjectName, s.app.ProjectRoot)
	}
}

func (s *utaShell) Sessions(w io.Writer, limit int) error {
	rows, err := s.app.Store.ListSessions(limit, 0, "")
	if err != nil {
		return err
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tCREATED\tWORKER\tSTATUS\tGOAL")
	for _, row := range rows {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n",
			shortID(row.ID), row.CreatedAt.Format(time.RFC3339),
			row.Worker, row.Status, truncateLine(row.Goal, 60))
	}
	return tw.Flush()
}

func (s *utaShell) Agents(ctx context.Context, w io.Writer) error {
	detections := s.app.Registry.DetectAll(ctx)
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "  NAME\tAVAILABLE\tMODEL\tNOTES")
	for _, n := range s.app.Registry.Names() {
		marker := " "
		if n == s.worker {
			marker = "*"
		}
		model := "-"
		if m := s.models[n]; m != "" {
			if _, v, ok := strings.Cut(m, "="); ok {
				model = v
			}
		}
		det := detections[n]
		fmt.Fprintf(tw, "%s %s\t%v\t%s\t%s\n", marker, n, det.Available, model, det.Notes)
	}
	return tw.Flush()
}

func (s *utaShell) LocalExec(ctx context.Context, cmdline string) error {
	sh, flag := "/bin/sh", "-c"
	if runtime.GOOS == "windows" {
		sh, flag = "cmd", "/C"
	}
	c := exec.CommandContext(ctx, sh, flag, cmdline)
	c.Dir = s.workdir
	c.Stdout = s.out
	c.Stderr = s.errw
	return c.Run()
}

func (s *utaShell) ExecTurn(parent context.Context, goal string) error {
	return s.execTurn(parent, s.worker, goal)
}

func (s *utaShell) ExecTurnAs(parent context.Context, worker, goal string) error {
	detections := s.app.Registry.DetectAll(parent)
	available := availableProviders(s.app.Registry.Names(), detections)
	if !containsString(available, worker) {
		det := detections[worker]
		return fmt.Errorf("provider %q is not available: %s", worker, firstNonEmptyStr(det.Notes, "not detected"))
	}
	return s.execTurn(parent, worker, goal)
}

// execTurn runs one conversational turn. No attached session → a fresh
// orchestration run (planner → fan-out → synthesis); attached → resume,
// which carries the prior context. Either way the resulting session becomes
// the one the next turn continues.
func (s *utaShell) execTurn(parent context.Context, worker, goal string) error {
	ctx, stop := turnSignalContext(parent)
	defer stop()

	bus := trajectory.NewBus()
	rdr := NewRenderer(s.errw, s.noColor)
	rdr.ShowGoal(goal, worker)
	renderDone := rdr.Subscribe(bus)

	sup := engine.New(engine.Deps{
		Store:      s.app.Store,
		Blobs:      s.app.Blobs,
		Recorder:   trajectory.NewRecorder(s.app.Store),
		Bus:        bus,
		Registry:   s.app.Registry,
		Memory:     memory.NewFromEnv(s.app.Store.DB),
		Whiteboard: whiteboard.New(s.app.Store.DB),
	})

	env := s.app.ProjectSubtaskEnv()
	// The /model pin is appended after profile env merging (below) so an
	// explicit shell-side pin wins over a mode's env on duplicate keys.
	var modelEnv []string
	if m := s.models[worker]; m != "" {
		modelEnv = []string{m}
	}
	gate := &hitl.Gate{StateDir: s.app.StateDir, Stdin: s.stdin, Stderr: s.errw}

	var result engine.RunResult
	var err error
	if s.sessionID == "" {
		req := engine.RunRequest{
			Goal:            goal,
			WorkerName:      worker,
			MaxParallel:     s.maxParallel,
			MaxSubtasks:     s.maxSubtasks,
			SubtaskTimeout:  s.subtaskTimeout,
			RunTimeout:      s.runTimeout,
			PreApproveTools: s.preApprove,
			Workdir:         s.workdir,
			Env:             env,
			HITL:            gate,
		}
		engine.ApplyProfile(&req, s.mode)
		req.Env = append(req.Env, modelEnv...)
		if s.mode != nil && len(s.mode.MCPServers) > 0 {
			warnMCPProbes(s.errw, engine.ApplyMCPBridge(ctx, &req, s.mode))
			if req.MCPConfigPath != "" {
				defer os.Remove(req.MCPConfigPath)
			}
		}
		result, err = sup.Run(ctx, req)
	} else {
		req := engine.ResumeRequest{
			PriorSessionID:  s.sessionID,
			Goal:            goal,
			WorkerName:      worker,
			SubtaskTimeout:  s.subtaskTimeout,
			RunTimeout:      s.runTimeout,
			PreApproveTools: s.preApprove,
			Workdir:         s.workdir,
			Env:             env,
			HITL:            gate,
		}
		engine.ApplyProfileToResume(&req, s.mode)
		req.Env = append(req.Env, modelEnv...)
		if s.mode != nil && len(s.mode.MCPServers) > 0 {
			warnMCPProbes(s.errw, engine.ApplyMCPBridgeToResume(ctx, &req, s.mode))
			if req.MCPConfigPath != "" {
				defer os.Remove(req.MCPConfigPath)
			}
		}
		result, err = sup.Resume(ctx, req)
	}

	if result.SessionID != "" {
		s.sessionID = result.SessionID
		if terr := state.TouchActiveSession(s.app.StateDir, result.SessionID); terr != nil {
			fmt.Fprintln(s.errw, "warn: update active thread:", terr)
		}
	}

	bus.Shutdown()
	<-renderDone

	if err != nil {
		// Ctrl-C cancels the turn, not the shell: stay attached so the next
		// input resumes the partially-run session.
		if errors.Is(err, context.Canceled) && parent.Err() == nil {
			fmt.Fprintln(s.errw, "turn cancelled.")
			if result.SessionID != "" {
				fmt.Fprintf(s.errw, "still attached to %s — next input resumes it, /new starts fresh.\n", shortID(result.SessionID))
			}
			return nil
		}
		return err
	}
	if result.FinalAnswer != "" {
		fmt.Fprintln(s.out, result.FinalAnswer)
	}
	fmt.Fprintf(s.errw, "\n[uta] session %s status=%s%s\n", shortID(result.SessionID), result.Status, usageSummary(result.Subtasks))
	return nil
}

// RunMissionFile interprets a .steer program from inside the shell. The
// mission gets the shell's default worker, workdir, and mode env, but is
// its own session — the attached conversation is not consumed.
func (s *utaShell) RunMissionFile(parent context.Context, path string, checkOnly bool) error {
	prog, warnings, ok := loadSteerProgram(path, s.errw)
	if !ok {
		return errors.New("program did not compile (fix the diagnostics above)")
	}
	if checkOnly {
		suffix := ""
		if warnings > 0 {
			suffix = fmt.Sprintf(", %d warning(s)", warnings)
		}
		fmt.Fprintf(s.out, "ok: mission %q — %d agent fn(s), %d call(s), budget %s%s\n",
			prog.Mission.Name, len(prog.Agents), len(prog.Mission.Calls()), formatBudget(prog.Mission.Budget), suffix)
		return nil
	}

	ctx, stop := turnSignalContext(parent)
	defer stop()

	detections := s.app.Registry.DetectAll(ctx)
	available := availableProviders(s.app.Registry.Names(), detections)

	bus := trajectory.NewBus()
	rdr := NewRenderer(s.errw, s.noColor)
	rdr.ShowGoal("mission "+prog.Mission.Name, s.worker)
	renderDone := rdr.Subscribe(bus)

	sup := engine.New(engine.Deps{
		Store:    s.app.Store,
		Blobs:    s.app.Blobs,
		Recorder: trajectory.NewRecorder(s.app.Store),
		Bus:      bus,
		Registry: s.app.Registry,
	})

	var rr engine.RunRequest
	engine.ApplyProfile(&rr, s.mode)
	env := append(rr.Env, s.app.ProjectSubtaskEnv()...)

	res, err := sup.RunMission(ctx, engine.MissionRequest{
		Program:       prog,
		SourceFile:    filepath.Base(path),
		DefaultWorker: s.worker,
		Available:     available,
		Workdir:       s.workdir,
		Env:           env,
		ModeName:      rr.ModeName,
	})

	bus.Shutdown()
	<-renderDone

	if err != nil {
		if errors.Is(err, context.Canceled) && parent.Err() == nil {
			fmt.Fprintln(s.errw, "mission cancelled.")
			if res.SessionID != "" {
				fmt.Fprintf(s.errw, "session: %s\n", shortID(res.SessionID))
			}
			return nil
		}
		return err
	}
	if res.FinalAnswer != "" {
		fmt.Fprintln(s.out, res.FinalAnswer)
	}
	fmt.Fprintf(s.errw, "\n[uta] mission %s session %s status=%s calls=%d%s\n",
		prog.Mission.Name, shortID(res.SessionID), res.Status, len(res.Subtasks), usageSummary(res.Subtasks))
	return nil
}

func warnMCPProbes(w io.Writer, probes []engine.MCPProbeResult) {
	for _, p := range probes {
		if diag := engine.FormatMCPProbeError(p); diag != "" {
			fmt.Fprintln(w, "warn:", diag)
		}
	}
}

// turnSignalContext catches SIGINT/SIGTERM for the duration of one turn so
// Ctrl-C cancels the in-flight run instead of killing the shell. The stop
// function uninstalls the handler, restoring default signal behavior at the
// prompt (where Ctrl-C exiting the process is the predictable outcome).
func turnSignalContext(parent context.Context) (context.Context, func()) {
	ctx, cancel := context.WithCancel(parent)
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGINT, syscall.SIGTERM)
	done := make(chan struct{})
	go func() {
		defer close(done)
		select {
		case <-ctx.Done():
		case <-ch:
			cancel()
		}
	}()
	return ctx, func() {
		signal.Stop(ch)
		cancel()
		<-done
	}
}
