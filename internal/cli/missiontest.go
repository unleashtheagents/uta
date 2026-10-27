package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	"github.com/unleashtheagents/uta/internal/engine"
	"github.com/unleashtheagents/uta/internal/hitl"
	"github.com/unleashtheagents/uta/internal/profile"
	"github.com/unleashtheagents/uta/internal/provider"
	"github.com/unleashtheagents/uta/internal/steer"
	"github.com/unleashtheagents/uta/internal/store"
	"github.com/unleashtheagents/uta/internal/trajectory"
)

// missionScript is the parsed --script file: canned response texts per agent
// fn (a list is served in call order), plus an optional expected final emit
// and optional golden-journal assertions.
//
//	responses:
//	  greet:
//	    - "hello, world"
//	expect: "hello, world"
//	assert:
//	  calls:
//	    greet: 1
//	  gates_fired: 1
//	  max_cost_usd: 0.5
type missionScript struct {
	Responses map[string]scriptQueue `yaml:"responses"`
	Expect    string                 `yaml:"expect"`
	Assert    *missionAssert         `yaml:"assert"`
}

// missionAssert is the optional golden-journal assertion block, checked
// after the mission completes. Pointer fields distinguish "assert zero"
// from "not asserted".
type missionAssert struct {
	// Calls pins the number of times each named agent fn was invoked.
	Calls map[string]int `yaml:"calls"`
	// GatesFired pins how many `gate human` statements executed (their
	// journal rows carry a "-gate" structural spec id).
	GatesFired *int `yaml:"gates_fired"`
	// MaxCostUSD caps the summed per-call usd_cents usage on the journal.
	MaxCostUSD *float64 `yaml:"max_cost_usd"`
}

// failures evaluates the assert block against the run: per-fn call counts
// from the scripted serving state, gate rows and per-call usage meta from
// the journaled subtasks. Returns one want/got line per failed assertion.
func (a *missionAssert) failures(calls map[string]int, subs []store.Subtask) []string {
	if a == nil {
		return nil
	}
	var fails []string
	names := make([]string, 0, len(a.Calls))
	for name := range a.Calls {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if got := calls[name]; got != a.Calls[name] {
			fails = append(fails, fmt.Sprintf("calls[%s]: want %d, got %d", name, a.Calls[name], got))
		}
	}
	if a.GatesFired != nil {
		got := 0
		for _, s := range subs {
			if strings.HasSuffix(s.SpecID, "-gate") {
				got++
			}
		}
		if got != *a.GatesFired {
			fails = append(fails, fmt.Sprintf("gates_fired: want %d, got %d", *a.GatesFired, got))
		}
	}
	if a.MaxCostUSD != nil {
		var cents int64
		for _, s := range subs {
			if s.MetaJSON == "" {
				continue
			}
			var m struct {
				USDCents int64 `json:"usd_cents"`
			}
			if json.Unmarshal([]byte(s.MetaJSON), &m) == nil {
				cents += m.USDCents
			}
		}
		if got := float64(cents) / 100; got > *a.MaxCostUSD {
			fails = append(fails, fmt.Sprintf("max_cost_usd: want <= %v, got %v", *a.MaxCostUSD, got))
		}
	}
	return fails
}

// scriptQueue accepts either a single string or a list of strings in YAML,
// so one-call fns don't need list syntax.
type scriptQueue []string

func (q *scriptQueue) UnmarshalYAML(node *yaml.Node) error {
	switch node.Kind {
	case yaml.ScalarNode:
		var s string
		if err := node.Decode(&s); err != nil {
			return err
		}
		*q = scriptQueue{s}
		return nil
	case yaml.SequenceNode:
		var ss []string
		if err := node.Decode(&ss); err != nil {
			return err
		}
		*q = scriptQueue(ss)
		return nil
	}
	return fmt.Errorf("line %d: a responses entry must be a string or a list of strings", node.Line)
}

// loadMissionScript reads + validates the script against the program: every
// scripted name must be a declared agent fn, so a typo fails loudly instead
// of silently starving the fn it meant to feed.
func loadMissionScript(path string, prog *steer.Program) (*missionScript, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var script missionScript
	dec := yaml.NewDecoder(strings.NewReader(string(data)))
	dec.KnownFields(true)
	if err := dec.Decode(&script); err != nil {
		return nil, fmt.Errorf("%s: %w", filepath.Base(path), err)
	}
	declared := make([]string, len(prog.Agents))
	for i, a := range prog.Agents {
		declared[i] = a.Name
	}
	sort.Strings(declared)
	for name := range script.Responses {
		if prog.Agent(name) == nil {
			return nil, fmt.Errorf("%s: script responds for %q, but the program declares no such agent fn (declared: %s)",
				filepath.Base(path), name, strings.Join(declared, ", "))
		}
	}
	if script.Assert != nil {
		for name := range script.Assert.Calls {
			if prog.Agent(name) == nil {
				return nil, fmt.Errorf("%s: script asserts calls for %q, but the program declares no such agent fn (declared: %s)",
					filepath.Base(path), name, strings.Join(declared, ", "))
			}
		}
	}
	return &script, nil
}

// scriptFromSession converts a recorded mission session's journal into a
// missionScript — the M6 "production trajectory becomes a regression test"
// path. Every completed agent call is attributed back to its agent fn by
// matching the recorded prompt (from the prompt_ref blob) against the
// program's templates, and its full result text is recovered the same way
// --resume-session does: the result_ref blob first, the untruncated
// ResultText as fallback. Responses queue per fn in journal (ord) order;
// gate rows are skipped (gates auto-approve under test) and the session's
// final answer becomes the script's expect.
func scriptFromSession(prog *steer.Program, sess store.Session, subs []store.Subtask, blobs *store.Blobs) (*missionScript, error) {
	matchers := make([]scriptMatcher, len(prog.Agents))
	for i, fn := range prog.Agents {
		matchers[i] = newScriptMatcher(fn)
	}
	script := &missionScript{Responses: map[string]scriptQueue{}}
	for _, s := range subs {
		if s.Status != "completed" || strings.HasSuffix(s.SpecID, "-gate") {
			continue
		}
		prompt, ok := blobText(blobs, s.PromptRef)
		if !ok {
			return nil, fmt.Errorf("session %s: subtask %s (%s): prompt blob %q is unreadable", sess.ID, s.SpecID, s.Title, s.PromptRef)
		}
		fn, ok := matchAgentFn(matchers, prompt)
		if !ok {
			return nil, fmt.Errorf("session %s: subtask %s (%s): recorded prompt matches no agent fn's template — has the program changed since the recording?", sess.ID, s.SpecID, s.Title)
		}
		text, ok := recoverSubtaskResult(s, blobs)
		if !ok {
			return nil, fmt.Errorf("session %s: subtask %s (%s): result text is unrecoverable (no result_ref blob and ResultText is truncated)", sess.ID, s.SpecID, s.Title)
		}
		script.Responses[fn] = append(script.Responses[fn], text)
	}
	if len(script.Responses) == 0 {
		return nil, fmt.Errorf("session %s has no completed agent calls to replay", sess.ID)
	}
	if final, ok := blobText(blobs, sess.FinalAnswerRef); ok {
		script.Expect = final
	}
	return script, nil
}

// recoverSubtaskResult retrieves a completed subtask's full result text: the
// result_ref blob its call meta recorded, or ResultText when that is
// certainly untruncated — the same recovery contract the engine's resume
// path applies.
func recoverSubtaskResult(s store.Subtask, blobs *store.Blobs) (string, bool) {
	var meta struct {
		ResultRef string `json:"result_ref"`
	}
	if s.MetaJSON != "" {
		_ = json.Unmarshal([]byte(s.MetaJSON), &meta)
	}
	if text, ok := blobText(blobs, meta.ResultRef); ok {
		return text, true
	}
	if len(s.ResultText) < 8000 { // definitely not truncated
		return s.ResultText, true
	}
	return "", false
}

func blobText(blobs *store.Blobs, ref string) (string, bool) {
	if ref == "" {
		return "", false
	}
	var buf strings.Builder
	if err := blobs.Get(ref, &buf); err != nil {
		return "", false
	}
	return buf.String(), true
}

// slotRe matches ${name} interpolation slots in a prompt template.
var slotRe = regexp.MustCompile(`\$\{[A-Za-z_][A-Za-z0-9_]*\}`)

// scriptMatcher attributes a rendered prompt back to the agent fn whose
// template produced it: the template's literal chunks become an anchored
// regex with slots relaxed to `.*?`. litLen breaks ties toward the most
// specific template when one template's literals prefix another's.
type scriptMatcher struct {
	fn     string
	re     *regexp.Regexp
	litLen int
}

func newScriptMatcher(fn *steer.AgentFn) scriptMatcher {
	parts := slotRe.Split(fn.Prompt.Text, -1)
	quoted := make([]string, len(parts))
	litLen := 0
	for i, p := range parts {
		quoted[i] = regexp.QuoteMeta(p)
		litLen += len(p)
	}
	return scriptMatcher{
		fn:     fn.Name,
		re:     regexp.MustCompile(`(?s)^` + strings.Join(quoted, `.*?`)),
		litLen: litLen,
	}
}

// missionScriptState is the shared serving state behind every scripted
// provider alias: matchers to attribute prompts to fns, one FIFO response
// queue per fn, and per-fn call counters for error messages. Calls may run
// in parallel (par branches), hence the mutex.
type missionScriptState struct {
	mu       sync.Mutex
	matchers []scriptMatcher
	queues   map[string][]string
	calls    map[string]int
}

func newMissionScriptState(prog *steer.Program, script *missionScript) *missionScriptState {
	st := &missionScriptState{
		queues: make(map[string][]string, len(script.Responses)),
		calls:  make(map[string]int, len(prog.Agents)),
	}
	for _, fn := range prog.Agents {
		st.matchers = append(st.matchers, newScriptMatcher(fn))
	}
	for name, q := range script.Responses {
		st.queues[name] = append([]string{}, q...)
	}
	return st
}

// matchAgentFn attributes a rendered prompt to the agent fn whose template
// produced it, preferring the template with the most literal text on ties.
func matchAgentFn(matchers []scriptMatcher, prompt string) (string, bool) {
	best := -1
	for i, m := range matchers {
		if m.re.MatchString(prompt) && (best < 0 || m.litLen > matchers[best].litLen) {
			best = i
		}
	}
	if best < 0 {
		return "", false
	}
	return matchers[best].fn, true
}

// respond serves the next canned response for whichever agent fn the prompt
// renders from. A missing entry or an exhausted list is a semantic failure
// (wrapping ErrWorkerFailed, so the mission's failover loop doesn't burn
// through the other provider aliases — they all read this same script).
func (s *missionScriptState) respond(prompt string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	name, ok := matchAgentFn(s.matchers, prompt)
	if !ok {
		return "", fmt.Errorf("prompt matches no agent fn's template: %w", provider.ErrWorkerFailed)
	}
	s.calls[name]++
	q := s.queues[name]
	if len(q) == 0 {
		return "", fmt.Errorf("no scripted response for agent fn %q (call #%d): add it to the --script file: %w",
			name, s.calls[name], provider.ErrWorkerFailed)
	}
	s.queues[name] = q[1:]
	return q[0], nil
}

// unconsumed reports, one sorted line per fn, every scripted response queue
// the run left non-empty. A leftover response means the mission called the
// fn fewer times than the script's author provisioned for — usually a
// mission edit that dropped a call the test was written to cover.
func (s *missionScriptState) unconsumed(script *missionScript) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	names := make([]string, 0, len(script.Responses))
	for name := range script.Responses {
		names = append(names, name)
	}
	sort.Strings(names)
	var lines []string
	for _, name := range names {
		if left := len(s.queues[name]); left > 0 {
			declared := len(script.Responses[name])
			lines = append(lines, fmt.Sprintf("script declared %d response(s) for fn %q but only %d were consumed",
				declared, name, declared-left))
		}
	}
	return lines
}

// callCounts snapshots the per-fn call counters for post-run assertions.
func (s *missionScriptState) callCounts() map[string]int {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]int, len(s.calls))
	for name, n := range s.calls {
		out[name] = n
	}
	return out
}

// scriptedProvider is an AgentProvider that answers from the script instead
// of a CLI — no binary, no network, no tokens. One instance is registered
// per worker name the program mentions, all sharing the same state, so
// `worker any(claude, gemini)` clauses resolve without anything on PATH.
type scriptedProvider struct {
	name  string
	state *missionScriptState
}

func (p *scriptedProvider) Name() string { return p.name }

func (p *scriptedProvider) Detect(context.Context) provider.Detection {
	return provider.Detection{Available: true, BinaryPath: "(scripted)", Version: "mission-test", Notes: "scripted by uta mission test"}
}

func (p *scriptedProvider) RunHeadless(_ context.Context, prompt string, _ provider.RunOptions, _ chan<- provider.Event) (provider.RunResult, error) {
	text, err := p.state.respond(prompt)
	if err != nil {
		return provider.RunResult{}, err
	}
	return provider.RunResult{FinalText: text}, nil
}

// scriptedWorkerNames is the registry roster for a test run: the default
// worker plus every name the program's worker clauses mention.
func scriptedWorkerNames(prog *steer.Program) []string {
	const def = "scripted"
	seen := map[string]bool{def: true}
	names := []string{def}
	for _, fn := range prog.Agents {
		for _, w := range fn.Workers {
			if !seen[w] {
				seen[w] = true
				names = append(names, w)
			}
		}
	}
	return names
}

func newMissionTestCmd() *cobra.Command {
	var scriptPath, fromSession string
	cmd := &cobra.Command{
		Use:   "test <file.steer>",
		Short: "run a steer program against scripted responses — no provider, no network, no tokens",
		Long: `Interprets the program exactly like 'uta mission run', but every agent fn
call is answered from a YAML script instead of a real provider, against a
throwaway journal. 'gate human' statements auto-approve. The test passes
when the mission completes, (if the script declares 'expect:') the final
emit matches it, every 'assert:' clause holds against the journal, and
every scripted response was consumed — a leftover response fails the run,
since it usually means a mission edit dropped a call the script expected.

Script format — canned responses per agent fn (a list is served in call
order; a single string works for one-call fns), plus the optional
expected emit and golden-journal assertions:

  responses:
    greet:
      - "hello, world"
  expect: "hello, world"
  assert:
    calls:            # exact invocation count per agent fn
      greet: 1
    gates_fired: 1    # number of 'gate human' statements executed
    max_cost_usd: 0.5 # cap on summed per-call spend from the journal

--from-session derives the script from a recorded mission run instead:
each completed call's result is recovered from the session's journal and
served for the agent fn whose prompt template produced it, and the
session's final answer becomes the expected emit — a production
trajectory replayed as a regression test, with zero provider calls.`,
		Example: `  uta mission test hello.steer --script hello_responses.yaml
  uta mission test hello.steer --from-session 8f2c31a0`,
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: completeSteerFiles,
		RunE: func(cmd *cobra.Command, args []string) error {
			prog, _, _, ok := loadSteerProgram(args[0], cmd.ErrOrStderr())
			if !ok {
				return exitWith(2)
			}
			var script *missionScript
			if fromSession != "" {
				app, err := newApp(cmd.Context())
				if err != nil {
					return err
				}
				defer app.Close()
				id, err := app.Store.ResolveSessionID(fromSession)
				if err != nil {
					fmt.Fprintln(cmd.ErrOrStderr(), "error:", err)
					return exitWith(2)
				}
				sess, err := app.Store.GetSession(id)
				if err != nil {
					fmt.Fprintln(cmd.ErrOrStderr(), "error:", err)
					return exitWith(2)
				}
				subs, err := app.Store.SubtaskListBySession(id, 0, 0)
				if err != nil {
					fmt.Fprintln(cmd.ErrOrStderr(), "error:", err)
					return exitWith(2)
				}
				if script, err = scriptFromSession(prog, sess, subs, app.Blobs); err != nil {
					fmt.Fprintln(cmd.ErrOrStderr(), "error:", err)
					return exitWith(2)
				}
			} else {
				var err error
				if script, err = loadMissionScript(scriptPath, prog); err != nil {
					fmt.Fprintln(cmd.ErrOrStderr(), "error:", err)
					return exitWith(2)
				}
			}

			// The journal is real (missions require one) but throwaway:
			// a test run must not pollute the operator's session store.
			dir, err := os.MkdirTemp("", "uta-mission-test-*")
			if err != nil {
				return err
			}
			defer os.RemoveAll(dir)
			db, err := store.Open(filepath.Join(dir, "test.db"))
			if err != nil {
				return err
			}
			defer db.Close()

			state := newMissionScriptState(prog, script)
			registry := provider.NewRegistry()
			workers := scriptedWorkerNames(prog)
			for _, name := range workers {
				if err := registry.Register(&scriptedProvider{name: name, state: state}, true); err != nil {
					return err
				}
			}

			bus := trajectory.NewBus()
			defer bus.Shutdown()
			sup := engine.New(engine.Deps{
				Store:    db,
				Blobs:    store.NewBlobs(dir),
				Recorder: trajectory.NewRecorder(db),
				Bus:      bus,
				Registry: registry,
			})

			// A `policy P` mission normally resolves P against installed
			// profiles; the test double is an empty grant set — scripted
			// providers emit no tool calls, so the gate never fires.
			var policy *profile.MissionProfile
			if prog.Mission.Policy != nil {
				policy = &profile.MissionProfile{Name: prog.Mission.Policy.Name}
			}

			res, runErr := sup.RunMission(cmd.Context(), engine.MissionRequest{
				Program:       prog,
				SourceFile:    filepath.Base(args[0]),
				DefaultWorker: workers[0],
				Available:     workers,
				Policy:        policy,
				HITL:          &hitl.StaticApprover{Approve: true},
			})
			if runErr != nil {
				fmt.Fprintf(cmd.ErrOrStderr(), "FAIL: mission %s: %v\n", prog.Mission.Name, runErr)
				return exitWith(4)
			}
			if script.Expect != "" && strings.TrimSpace(res.FinalAnswer) != strings.TrimSpace(script.Expect) {
				fmt.Fprintf(cmd.ErrOrStderr(), "FAIL: mission %s emit mismatch\n  want: %q\n  got:  %q\n",
					prog.Mission.Name, strings.TrimSpace(script.Expect), strings.TrimSpace(res.FinalAnswer))
				return exitWith(1)
			}
			if fails := script.Assert.failures(state.callCounts(), res.Subtasks); len(fails) > 0 {
				fmt.Fprintf(cmd.ErrOrStderr(), "FAIL: mission %s assertion(s) failed:\n", prog.Mission.Name)
				for _, f := range fails {
					fmt.Fprintf(cmd.ErrOrStderr(), "  %s\n", f)
				}
				return exitWith(1)
			}
			if leftovers := state.unconsumed(script); len(leftovers) > 0 {
				fmt.Fprintf(cmd.ErrOrStderr(), "FAIL: mission %s left scripted response(s) unconsumed:\n", prog.Mission.Name)
				for _, l := range leftovers {
					fmt.Fprintf(cmd.ErrOrStderr(), "  %s\n", l)
				}
				return exitWith(3)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "ok: mission %s — %d call(s) served from script, status=%s\n",
				prog.Mission.Name, len(res.Subtasks), res.Status)
			return nil
		},
	}
	cmd.Flags().StringVar(&scriptPath, "script", "", "YAML file of canned responses per agent fn")
	cmd.Flags().StringVar(&fromSession, "from-session", "", "derive the script from a recorded mission session's journal (ID or unique prefix)")
	cmd.MarkFlagsOneRequired("script", "from-session")
	cmd.MarkFlagsMutuallyExclusive("script", "from-session")
	return cmd
}
