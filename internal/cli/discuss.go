package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/unleashtheagents/uta/internal/config"
	"github.com/unleashtheagents/uta/internal/engine"
	"github.com/unleashtheagents/uta/internal/trajectory"
)

func newDiscussCmd() *cobra.Command {
	var (
		topic       string
		agentsFlag  []string
		rounds      int
		personas    []string
		synthName   string
		noSynth     bool
		turnTimeout time.Duration
		runTimeout  time.Duration
		workdir     string
		outFile     string
		asJSON      bool
	)
	cmd := &cobra.Command{
		Use:   "discuss",
		Short: "have two agents debate a topic over several rounds, then synthesize the outcome",
		Long: `Run a structured debate between two agents — ideally two different
provider CLIs backed by two different LLMs (see docs/model-selection.md for
pinning e.g. claude to Claude Fable 5). The agents alternate turns: opening
statements, rebuttals, and closing statements in the final round. A moderator
pass then synthesizes agreements, disagreements, and a verdict.

The debate is recorded as a normal session: each turn is a subtask, so
'uta trajectory', 'uta perf', and 'uta sessions' work on it unchanged.`,
		Example: `  # two different LLMs argue a design decision (first two detected providers)
  uta discuss -t "Should we migrate our monolith to microservices?"

  # explicit line-up, roles, and length
  uta discuss -t "Rewrite in Rust?" --agents claude,gemini --personas advocate,skeptic --rounds 4

  # same provider on both sides (personas differentiate), no moderator pass
  uta discuss -t "tabs vs spaces" --agents claude,claude --personas "tabs zealot","spaces zealot" --no-synth

  # machine-readable transcript
  uta discuss -t "..." --json > debate.json`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if strings.TrimSpace(topic) == "" && len(args) > 0 {
				topic = strings.Join(args, " ")
			}
			if strings.TrimSpace(topic) == "" {
				return errors.New("--topic/-t is required")
			}
			if len(personas) > 2 {
				return errors.New("--personas takes at most 2 values (agent A, agent B)")
			}
			if rounds <= 0 {
				rounds = 3
			}

			app, err := newApp(cmd.Context())
			if err != nil {
				return err
			}
			defer app.Close()

			// Resolve the two participants: explicit --agents wins; otherwise
			// pick the first two distinct available providers.
			detections := app.Registry.DetectAll(cmd.Context())
			available := availableProviders(app.Registry.Names(), detections)
			agents, err := resolveDiscussionAgents(agentsFlag, available)
			if err != nil {
				if len(available) == 0 {
					fmt.Fprintln(cmd.ErrOrStderr(), errNoProviders)
					return exitWith(3)
				}
				return err
			}
			if agents[0] == agents[1] && len(personas) < 2 {
				fmt.Fprintln(cmd.ErrOrStderr(),
					"warn: both sides run on the same provider — pass --personas to differentiate them, or --agents a,b for two different LLMs")
			}

			// Personas: a value matching a ~/.uta/personas/ id expands to that
			// persona's prompt; anything else is used verbatim as the role.
			var personaTexts [2]string
			for i := range personas {
				personaTexts[i] = resolvePersonaText(personas[i], app.UserPersonas)
			}

			// Mode plumbing: reuse ApplyProfile via a scratch RunRequest so
			// --mode env + budget policies apply to discussions identically.
			mode, merr := resolveActiveMode(cmd, app)
			if merr != nil {
				return merr
			}
			var rr engine.RunRequest
			engine.ApplyProfile(&rr, mode)

			subtaskEnv := append(rr.Env, app.ProjectSubtaskEnv()...)
			if app.InProject() && workdir == "" {
				workdir = app.ProjectRoot
			}

			req := engine.DiscussionRequest{
				Topic: topic,
				Agents: [2]engine.DiscussionAgent{
					{Provider: agents[0], Persona: personaTexts[0]},
					{Provider: agents[1], Persona: personaTexts[1]},
				},
				Rounds:           rounds,
				TurnTimeout:      turnTimeout,
				RunTimeout:       runTimeout,
				Workdir:          workdir,
				Env:              subtaskEnv,
				SynthWorker:      synthName,
				SkipSynthesis:    noSynth,
				ModeName:         rr.ModeName,
				MaxTokens:        rr.MaxTokens,
				MaxUSDCents:      rr.MaxUSDCents,
				PerCallMaxTokens: rr.PerCallMaxTokens,
				AllowedTools:     rr.AllowedTools,
				DeniedTools:      rr.DeniedTools,
			}

			ctx, cancel := signalContext(cmd.Context())
			defer cancel()

			bus := trajectory.NewBus()
			defer bus.Shutdown()

			// Live transcript: headers to stderr, turn text to stdout as it
			// lands, budget alerts inline. JSON mode stays machine-clean.
			var renderDone <-chan struct{}
			if !asJSON {
				fmt.Fprintf(cmd.ErrOrStderr(), "→ %s\n  agents: %s vs %s, %d round(s)\n",
					topic, agents[0], agents[1], rounds)
				renderDone = renderDiscussion(bus, cmd.OutOrStdout(), cmd.ErrOrStderr())
			}

			deps := engine.Deps{
				Store:    app.Store,
				Blobs:    app.Blobs,
				Recorder: trajectory.NewRecorder(app.Store),
				Bus:      bus,
				Registry: app.Registry,
			}
			start := time.Now()
			res, runErr := engine.New(deps).Discuss(ctx, req)

			bus.Shutdown()
			if renderDone != nil {
				<-renderDone
			}

			if outFile != "" && len(res.Turns) > 0 {
				if werr := os.WriteFile(outFile, []byte(discussionMarkdown(topic, res)), 0o644); werr != nil {
					fmt.Fprintln(cmd.ErrOrStderr(), "warn: write --out:", werr)
				} else {
					fmt.Fprintf(cmd.ErrOrStderr(), "[uta] transcript written to %s\n", outFile)
				}
			}

			if runErr != nil {
				if errors.Is(runErr, context.Canceled) {
					fmt.Fprintln(cmd.ErrOrStderr(), "cancelled.")
					if res.SessionID != "" {
						fmt.Fprintf(cmd.ErrOrStderr(), "session: %s (%d turn(s) completed)\n",
							shortID(res.SessionID), len(res.Turns))
					}
					return exitWith(130)
				}
				fmt.Fprintf(cmd.ErrOrStderr(), "discussion failed: %v\n", runErr)
				if res.SessionID != "" {
					fmt.Fprintf(cmd.ErrOrStderr(), "session: %s (%d turn(s) completed — see 'uta trajectory %s')\n",
						shortID(res.SessionID), len(res.Turns), shortID(res.SessionID))
				}
				return exitWith(4)
			}

			if !asJSON && res.Synthesis != "" {
				fmt.Fprintf(cmd.OutOrStdout(), "---\n\n# Moderator synthesis\n\n%s\n", res.Synthesis)
			}

			if asJSON {
				enc := json.NewEncoder(cmd.OutOrStdout())
				enc.SetIndent("", "  ")
				if err := enc.Encode(struct {
					Topic  string   `json:"topic"`
					Agents []string `json:"agents"`
					engine.DiscussionResult
				}{topic, agents[:], res}); err != nil {
					return err
				}
			}

			usage := ""
			if tokens := res.TokensIn + res.TokensOut; tokens > 0 {
				usage = fmt.Sprintf(" tokens=%d", tokens)
				if res.USDCents > 0 {
					usage += fmt.Sprintf(" cost=$%.2f", float64(res.USDCents)/100)
				}
			}
			fmt.Fprintf(cmd.ErrOrStderr(), "\n[uta] session %s status=%s turns=%d duration=%s%s\n",
				res.SessionID, res.Status, len(res.Turns), time.Since(start).Round(time.Second), usage)
			return nil
		},
	}

	cmd.Flags().StringVarP(&topic, "topic", "t", "", "the topic / question the agents debate (required)")
	cmd.Flags().StringSliceVar(&agentsFlag, "agents", nil, "exactly two provider names, comma-separated (default: first two distinct available providers)")
	cmd.Flags().IntVar(&rounds, "rounds", 3, "full rounds (one turn per agent each); the last round is closing statements")
	cmd.Flags().StringSliceVar(&personas, "personas", nil, "role for each agent: a persona id from ~/.uta/personas/ or free text (e.g. advocate,skeptic)")
	cmd.Flags().StringVar(&synthName, "synth", "", "moderator provider for the final synthesis (defaults to the first agent)")
	cmd.Flags().BoolVar(&noSynth, "no-synth", false, "skip the moderator synthesis; the transcript is the output")
	cmd.Flags().DurationVar(&turnTimeout, "turn-timeout", 5*time.Minute, "per-turn timeout")
	cmd.Flags().DurationVar(&runTimeout, "timeout", 30*time.Minute, "overall debate timeout")
	cmd.Flags().StringVar(&workdir, "workdir", "", "working directory exposed to the agents (defaults to CWD / project root)")
	cmd.Flags().StringVarP(&outFile, "out", "o", "", "also write the full markdown transcript to this file")
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit the structured result as JSON on stdout (suppresses the live transcript)")
	return cmd
}

// resolveDiscussionAgents picks the two participants. Explicit --agents must
// name exactly two registered-and-available providers (the same one twice is
// allowed). With no flag, the first two distinct available providers are
// paired — two different LLMs by default.
func resolveDiscussionAgents(flag, available []string) ([2]string, error) {
	if len(flag) > 0 {
		if len(flag) != 2 {
			return [2]string{}, fmt.Errorf("--agents needs exactly 2 providers, got %d", len(flag))
		}
		for _, a := range flag {
			if !containsString(available, a) {
				return [2]string{}, fmt.Errorf("provider %q is not available (installed providers: %s)",
					a, strings.Join(available, ", "))
			}
		}
		return [2]string{flag[0], flag[1]}, nil
	}
	if len(available) >= 2 {
		return [2]string{available[0], available[1]}, nil
	}
	if len(available) == 1 {
		return [2]string{}, fmt.Errorf(
			"only one provider (%s) is available; a debate wants two different LLMs.\nInstall a second provider, or pass --agents %s,%s with --personas to run both sides on one",
			available[0], available[0], available[0])
	}
	return [2]string{}, errors.New("no providers available")
}

// resolvePersonaText expands a --personas value: if it matches a user
// persona's id, use that persona's prompt; otherwise the literal text is the
// role description.
func resolvePersonaText(v string, personas []*config.Persona) string {
	for _, p := range personas {
		if p != nil && p.ID == v {
			return p.Prompt
		}
	}
	return v
}

// renderDiscussion subscribes to the bus and prints the debate live: turn
// headers to errW (status), turn text to outW (the transcript is the
// product), budget alerts inline. Returns a channel closed once the bus is
// drained.
func renderDiscussion(bus *trajectory.Bus, outW, errW io.Writer) <-chan struct{} {
	done := make(chan struct{})
	ch := bus.Subscribe(256)
	go func() {
		defer close(done)
		for ev := range ch {
			switch ev.Kind {
			case trajectory.GoalReceived:
				fmt.Fprintf(errW, "  session=%s\n", shortID(ev.SessionID))
			case trajectory.DiscussionTurnStarted:
				var p struct {
					Round   int    `json:"round"`
					Rounds  int    `json:"rounds"`
					Agent   string `json:"agent"`
					Persona string `json:"persona"`
				}
				if json.Unmarshal(ev.Payload, &p) == nil {
					label := p.Agent
					if p.Persona != "" {
						label += " (" + truncateLine(p.Persona, 32) + ")"
					}
					fmt.Fprintf(errW, "  round %d/%d — %s is thinking…\n", p.Round, p.Rounds, label)
				}
			case trajectory.DiscussionTurnDone:
				var p struct {
					Round    int    `json:"round"`
					Agent    string `json:"agent"`
					AgentIdx int    `json:"agent_idx"`
					Persona  string `json:"persona"`
					Text     string `json:"text"`
				}
				if json.Unmarshal(ev.Payload, &p) == nil {
					fmt.Fprintf(outW, "%s\n\n%s\n\n", discussionTurnHeader(p.Round, p.AgentIdx, p.Agent, p.Persona), p.Text)
				}
			case trajectory.SynthesisStarted:
				fmt.Fprintln(errW, "  moderator is synthesizing…")
			case trajectory.SynthesisCompleted:
				var p struct {
					Error string `json:"error"`
				}
				if json.Unmarshal(ev.Payload, &p) == nil && p.Error != "" {
					fmt.Fprintf(errW, "  warn: synthesis failed (%s); transcript stands on its own\n", p.Error)
				}
			case trajectory.BudgetWarning, trajectory.BudgetExhausted:
				var p struct {
					Message string `json:"message"`
				}
				if json.Unmarshal(ev.Payload, &p) == nil && p.Message != "" {
					fmt.Fprintf(errW, "  ⚠ %s\n", p.Message)
				}
			case trajectory.DiscussionCompleted:
				// summary line printed by the command itself
			}
		}
	}()
	return done
}

func discussionTurnHeader(round, agentIdx int, agent, persona string) string {
	h := fmt.Sprintf("## Round %d — Agent %d: %s", round, agentIdx+1, agent)
	if persona != "" {
		h += " (" + truncateLine(persona, 48) + ")"
	}
	return h
}

// discussionMarkdown renders the full transcript + synthesis for --out.
func discussionMarkdown(topic string, res engine.DiscussionResult) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Debate: %s\n\nsession: %s\n\n", topic, res.SessionID)
	for _, t := range res.Turns {
		fmt.Fprintf(&b, "%s\n\n%s\n\n", discussionTurnHeader(t.Round, t.AgentIdx, t.Agent, t.Persona), t.Text)
	}
	if res.Synthesis != "" {
		fmt.Fprintf(&b, "---\n\n# Moderator synthesis\n\n%s\n", res.Synthesis)
	}
	return b.String()
}
