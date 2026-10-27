package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/mattn/go-isatty"

	"github.com/unleashtheagents/uta/internal/trajectory"
)

// Renderer subscribes to the trajectory bus and writes a friendly,
// dot-streaming progress log to stderr. Color is enabled iff stderr is a TTY
// and neither --no-color nor NO_COLOR disable it.
type Renderer struct {
	w        io.Writer
	color    bool
	mu       sync.Mutex
	phase    string
	started  time.Time
	doneSubs int
	// lastSession is the most recent session id announced to the user.
	// Handoff chains reuse one renderer across sessions, so each new id
	// gets its own line.
	lastSession string
	// Live usage accumulated from subtask_completed payloads, surfaced in
	// the final "Done" line so spend is visible without `uta perf`.
	tokensIn  int64
	tokensOut int64
	usdCents  int64
	// running tracks in-flight subtask ids; assistant text streams as
	// prose only while exactly one is running (dots otherwise).
	running map[string]bool
}

const (
	ansiReset  = "\x1b[0m"
	ansiBold   = "\x1b[1m"
	ansiDim    = "\x1b[2m"
	ansiGreen  = "\x1b[32m"
	ansiYellow = "\x1b[33m"
	ansiRed    = "\x1b[31m"
	ansiCyan   = "\x1b[36m"
)

// NewRenderer constructs a progress renderer. forceNoColor overrides
// auto-detection; pass true when --no-color is set.
func NewRenderer(w io.Writer, forceNoColor bool) *Renderer {
	color := !forceNoColor
	if os.Getenv("NO_COLOR") != "" {
		color = false
	}
	if f, ok := w.(*os.File); ok {
		if !isatty.IsTerminal(f.Fd()) {
			color = false
		}
	}
	return &Renderer{w: w, color: color, started: time.Now()}
}

// Subscribe attaches to bus and renders events as they arrive. The returned
// channel closes when the bus is shut down and the renderer has drained.
func (r *Renderer) Subscribe(bus *trajectory.Bus) <-chan struct{} {
	done := make(chan struct{})
	ch := bus.Subscribe(256)
	go func() {
		defer close(done)
		for ev := range ch {
			r.handle(ev)
		}
		r.finish()
	}()
	return done
}

// ShowGoal prints a leading "→ Goal: ..." line. Call once before the run starts.
func (r *Renderer) ShowGoal(goal, worker string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	one := strings.ReplaceAll(strings.TrimSpace(goal), "\n", " ")
	if len(one) > 96 {
		one = one[:95] + "…"
	}
	fmt.Fprintf(r.w, "%s %s\n", r.cyan("→"), r.bold(one))
	fmt.Fprintf(r.w, "%s\n", r.dim(fmt.Sprintf("  worker=%s", worker)))
}

func (r *Renderer) c(s, code string) string {
	if !r.color {
		return s
	}
	return code + s + ansiReset
}

func (r *Renderer) green(s string) string  { return r.c(s, ansiGreen) }
func (r *Renderer) red(s string) string    { return r.c(s, ansiRed) }
func (r *Renderer) yellow(s string) string { return r.c(s, ansiYellow) }
func (r *Renderer) cyan(s string) string   { return r.c(s, ansiCyan) }
func (r *Renderer) bold(s string) string   { return r.c(s, ansiBold) }
func (r *Renderer) dim(s string) string    { return r.c(s, ansiDim) }

// startPhase begins a new labeled phase. Caller must hold r.mu.
func (r *Renderer) startPhaseLocked(label string) {
	if r.phase != "" {
		// Force-close any prior unclosed phase with a quiet newline.
		fmt.Fprintln(r.w)
	}
	r.phase = label
	fmt.Fprintf(r.w, "  %s ", r.dim(label+"…"))
}

func (r *Renderer) endPhaseLocked(suffixGreen, suffixDim string) {
	if r.phase == "" {
		return
	}
	parts := []string{r.green("✓")}
	if suffixGreen != "" {
		parts = append(parts, r.green(suffixGreen))
	}
	if suffixDim != "" {
		parts = append(parts, r.dim(suffixDim))
	}
	fmt.Fprintf(r.w, " %s\n", strings.Join(parts, " "))
	r.phase = ""
}

func (r *Renderer) dotLocked(marker string) {
	if r.phase == "" {
		return
	}
	if marker == "" {
		marker = r.dim(".")
	}
	fmt.Fprint(r.w, marker)
}

// interjectLocked prints a full line mid-phase (budget warnings, alerts)
// without corrupting the dot stream: it breaks the current line, prints the
// message, and restarts the phase label so dots keep a home. Caller must
// hold r.mu.
func (r *Renderer) interjectLocked(line string) {
	if r.phase == "" {
		fmt.Fprintf(r.w, "  %s\n", line)
		return
	}
	fmt.Fprintf(r.w, "\n  %s\n", line)
	fmt.Fprintf(r.w, "  %s ", r.dim(r.phase+"…"))
}

// streamTextLocked prints one assistant text block as dim, gutter-marked
// prose inside the current phase, then restores the phase label so dots
// keep a home. Caller must hold r.mu and ensure a phase is active.
func (r *Renderer) streamTextLocked(text string) {
	fmt.Fprintln(r.w)
	for _, ln := range strings.Split(strings.TrimSpace(text), "\n") {
		fmt.Fprintf(r.w, "  %s %s\n", r.dim("│"), r.dim(ln))
	}
	fmt.Fprintf(r.w, "  %s ", r.dim(r.phase+"…"))
}

// usageSuffix renders the accumulated token/cost totals for the final
// summary line, or "" when nothing was recorded (provider didn't report
// usage). Caller must hold r.mu.
func (r *Renderer) usageSuffix() string {
	total := r.tokensIn + r.tokensOut
	if total == 0 && r.usdCents == 0 {
		return ""
	}
	s := fmt.Sprintf(", %s tokens", humanCount(total))
	if r.usdCents > 0 {
		s += fmt.Sprintf(", ~$%.2f", float64(r.usdCents)/100)
	}
	return s
}

// humanCount renders 12345 as "12.3k" — compact enough for a status line.
func humanCount(n int64) string {
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(n)/1_000_000)
	case n >= 1_000:
		return fmt.Sprintf("%.1fk", float64(n)/1_000)
	default:
		return fmt.Sprintf("%d", n)
	}
}

func (r *Renderer) handle(ev trajectory.Event) {
	r.mu.Lock()
	defer r.mu.Unlock()

	switch ev.Kind {
	case trajectory.GoalReceived:
		// Announce the session id up front so long runs can be followed
		// (uta trajectory <id>) or recovered without waiting for the end.
		if ev.SessionID != "" && ev.SessionID != r.lastSession {
			r.lastSession = ev.SessionID
			fmt.Fprintf(r.w, "%s\n", r.dim(fmt.Sprintf("  session=%s", shortID(ev.SessionID))))
		}
		r.startPhaseLocked("Planning")

	case trajectory.PlanProposed:
		var p struct {
			Subtasks []map[string]any `json:"subtasks"`
			Source   string           `json:"source"`
		}
		if err := json.Unmarshal(ev.Payload, &p); err != nil {
			r.endPhaseLocked("", r.yellow("plan payload unparseable"))
			r.doneSubs = 0
			r.startPhaseLocked("Running subtasks")
			return
		}
		n := len(p.Subtasks)
		suffix := fmt.Sprintf("%d subtask", n)
		if n != 1 {
			suffix += "s"
		}
		switch p.Source {
		case "workflow-yaml":
			suffix += " (from workflow)"
		case "direct":
			suffix = "direct turn"
		}
		r.endPhaseLocked(suffix, "")
		r.doneSubs = 0
		r.startPhaseLocked(fmt.Sprintf("Running %s", suffix))

	case trajectory.PlanFallback:
		r.endPhaseLocked("", r.yellow("planner output unparseable; using fallback"))
		r.startPhaseLocked("Running 1 subtask")

	case trajectory.SubtaskStarted:
		if ev.SubtaskID != "" {
			if r.running == nil {
				r.running = map[string]bool{}
			}
			r.running[ev.SubtaskID] = true
		}
		r.dotLocked("")

	case trajectory.SubtaskAssistantText:
		// Single-stream case: when exactly one subtask is in flight, show
		// the worker's actual prose as it lands instead of a dot. With
		// parallel subtasks the streams would interleave, so they stay dots.
		if r.phase != "" && len(r.running) == 1 && r.running[ev.SubtaskID] {
			var p struct {
				Text string `json:"text"`
			}
			if err := json.Unmarshal(ev.Payload, &p); err == nil && strings.TrimSpace(p.Text) != "" {
				r.streamTextLocked(p.Text)
				return
			}
		}
		r.dotLocked("")

	case trajectory.SubtaskToolCall:
		// Single-stream case: name the tool and its argument so the user
		// sees what the agent is doing, Claude Code style. Parallel runs
		// keep dots.
		if r.phase != "" && len(r.running) == 1 && r.running[ev.SubtaskID] {
			var p struct {
				Tool      string `json:"tool_name"`
				Arguments string `json:"arguments"`
			}
			if err := json.Unmarshal(ev.Payload, &p); err == nil && p.Tool != "" {
				r.toolCallLocked(p.Tool, toolArgSummary(p.Arguments))
				return
			}
		}
		r.dotLocked("")

	case trajectory.SubtaskToolResult:
		if r.phase != "" && len(r.running) == 1 && r.running[ev.SubtaskID] {
			var p struct {
				Tool    string `json:"tool_name"`
				Output  string `json:"output"`
				IsError bool   `json:"is_error"`
				Diff    string `json:"diff"`
				Path    string `json:"path"`
			}
			if err := json.Unmarshal(ev.Payload, &p); err == nil {
				switch {
				case p.Diff != "":
					r.diffLocked(p.Diff)
					return
				case p.IsError:
					r.interjectLocked(r.red("  ✗ " + p.Tool + ": " + firstLine(p.Output)))
					return
				}
			}
		}
		r.dotLocked("")

	case trajectory.SubtaskStdout:
		r.dotLocked("")

	case trajectory.SubtaskCompleted:
		r.doneSubs++
		delete(r.running, ev.SubtaskID)
		var p struct {
			TokensIn  int64 `json:"tokens_in"`
			TokensOut int64 `json:"tokens_out"`
			USDCents  int64 `json:"usd_cents"`
		}
		if err := json.Unmarshal(ev.Payload, &p); err == nil {
			r.tokensIn += p.TokensIn
			r.tokensOut += p.TokensOut
			r.usdCents += p.USDCents
		}
		r.dotLocked(r.green("•"))

	case trajectory.SubtaskFailed:
		delete(r.running, ev.SubtaskID)
		r.dotLocked(r.red("!"))

	case trajectory.SubtaskSkipped:
		delete(r.running, ev.SubtaskID)
		r.dotLocked(r.yellow("s"))

	case trajectory.BudgetWarning:
		var p struct {
			Message string `json:"message"`
		}
		msg := "budget 80% consumed"
		if err := json.Unmarshal(ev.Payload, &p); err == nil && p.Message != "" {
			msg = p.Message
		}
		r.interjectLocked(r.yellow("⚠ " + msg))

	case trajectory.BudgetExhausted:
		var p struct {
			Message string `json:"message"`
		}
		msg := "budget exhausted"
		if err := json.Unmarshal(ev.Payload, &p); err == nil && p.Message != "" {
			msg = "budget exhausted: " + p.Message
		}
		r.interjectLocked(r.red("✗ " + msg))

	case trajectory.CapabilityGateDenied:
		// A tool call the permissions gate or the active MissionProfile's
		// capability gate denied. Name it; the full payload is in the trajectory.
		var p struct {
			Tool    string `json:"tool"`
			Summary string `json:"summary"`
			Reason  string `json:"reason"`
			Source  string `json:"source"`
		}
		if err := json.Unmarshal(ev.Payload, &p); err == nil && p.Tool != "" {
			line := fmt.Sprintf("denied %s", p.Tool)
			if p.Summary != "" {
				line += ": " + truncateRunes(p.Summary, 80)
			}
			if p.Reason != "" {
				line += " (" + truncateRunes(p.Reason, 100) + ")"
			}
			r.interjectLocked(r.yellow("⊘ " + line))
			return
		}
		r.dotLocked(r.yellow("x"))

	case trajectory.SynthesisStarted:
		r.endPhaseLocked("", "")
		r.startPhaseLocked("Synthesizing")

	case trajectory.SynthesisCompleted:
		var p struct {
			Chars int    `json:"chars"`
			Mode  string `json:"mode"`
			Error string `json:"error"`
		}
		if err := json.Unmarshal(ev.Payload, &p); err != nil {
			r.endPhaseLocked("", r.yellow("synth payload unparseable"))
			return
		}
		switch {
		case p.Error != "":
			r.endPhaseLocked("", r.yellow("synth failed; joined subtask outputs"))
		case p.Mode == "skip":
			r.endPhaseLocked("", r.dim("(skip mode)"))
		default:
			r.endPhaseLocked("", r.dim(fmt.Sprintf("%d chars", p.Chars)))
		}

	case trajectory.RunCompleted:
		var p struct {
			Status   string `json:"status"`
			Subtasks int    `json:"subtasks"`
		}
		elapsed := time.Since(r.started).Round(time.Second)
		usage := r.usageSuffix()
		if err := json.Unmarshal(ev.Payload, &p); err != nil {
			fmt.Fprintf(r.w, "%s %s\n\n", r.green("✓"),
				r.bold(r.green(fmt.Sprintf("Done in %s%s", elapsed, usage))))
			return
		}
		switch p.Status {
		case "partial":
			fmt.Fprintf(r.w, "%s %s\n\n", r.yellow("◐"),
				r.bold(r.yellow(fmt.Sprintf("Partial: %d subtasks, some failed (%s%s)", p.Subtasks, elapsed, usage))))
		default:
			fmt.Fprintf(r.w, "%s %s\n\n", r.green("✓"),
				r.bold(r.green(fmt.Sprintf("Done in %s%s", elapsed, usage))))
		}

	case trajectory.RunFailed:
		r.endPhaseLocked("", "")
		fmt.Fprintf(r.w, "%s %s\n\n", r.red("✗"), r.bold(r.red("Run failed")))

	case trajectory.RunCancelled:
		r.endPhaseLocked("", "")
		fmt.Fprintf(r.w, "%s %s\n\n", r.yellow("×"), r.bold(r.yellow("Run cancelled")))
	}
}

// finish flushes any leftover state when the bus closes mid-run.
func (r *Renderer) finish() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.phase != "" {
		fmt.Fprintln(r.w)
		r.phase = ""
	}
}

// toolArgSummary pulls the one argument worth showing for a tool call.
func toolArgSummary(arguments string) string {
	var args map[string]any
	if json.Unmarshal([]byte(arguments), &args) != nil {
		return ""
	}
	for _, k := range []string{"command", "path", "query", "pattern"} {
		if v, ok := args[k].(string); ok && strings.TrimSpace(v) != "" {
			return truncateRunes(strings.TrimSpace(v), 100)
		}
	}
	if v, ok := args["urls"].([]any); ok && len(v) > 0 {
		if u, ok := v[0].(string); ok {
			return truncateRunes(u, 100)
		}
	}
	// MCP and other tools: show the compact JSON so the user sees what was sent.
	if len(args) > 0 {
		if b, err := json.Marshal(args); err == nil {
			return truncateRunes(string(b), 100)
		}
	}
	return ""
}

func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// toolCallLocked prints "  ⚙ run_command go test ./..." on its own line.
func (r *Renderer) toolCallLocked(tool, arg string) {
	fmt.Fprintln(r.w)
	if arg != "" {
		fmt.Fprintf(r.w, "  %s %s %s\n", r.dim("⚙"), r.bold(tool), r.dim(arg))
	} else {
		fmt.Fprintf(r.w, "  %s %s\n", r.dim("⚙"), r.bold(tool))
	}
}

// diffLocked prints a unified diff with +/- coloring, capped so a large
// rewrite does not flood the terminal.
func (r *Renderer) diffLocked(diff string) {
	const maxLines = 60
	lines := strings.Split(strings.TrimRight(diff, "\n"), "\n")
	for i, ln := range lines {
		if i >= maxLines {
			fmt.Fprintf(r.w, "    %s\n", r.dim(fmt.Sprintf("… %d more diff lines (/diff shows all)", len(lines)-maxLines)))
			break
		}
		switch {
		case strings.HasPrefix(ln, "+++"), strings.HasPrefix(ln, "---"):
			fmt.Fprintf(r.w, "    %s\n", r.bold(ln))
		case strings.HasPrefix(ln, "@@"):
			fmt.Fprintf(r.w, "    %s\n", r.cyan(ln))
		case strings.HasPrefix(ln, "+"):
			fmt.Fprintf(r.w, "    %s\n", r.green(ln))
		case strings.HasPrefix(ln, "-"):
			fmt.Fprintf(r.w, "    %s\n", r.red(ln))
		default:
			fmt.Fprintf(r.w, "    %s\n", r.dim(ln))
		}
	}
}
