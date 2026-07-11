package engine

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/unleashtheagents/uta/internal/budget"
	"github.com/unleashtheagents/uta/internal/provider"
	"github.com/unleashtheagents/uta/internal/trajectory"
)

// scriptedAgent registers a fakeProvider that records every prompt it
// receives and replies with "<name> says N" (N = call ordinal).
func scriptedAgent(t *testing.T, deps Deps, name string) *[]string {
	t.Helper()
	var prompts []string
	var n atomic.Int32
	p := &fakeProvider{
		name: name,
		run: func(_ context.Context, prompt string, _ provider.RunOptions, _ chan<- provider.Event) (provider.RunResult, error) {
			prompts = append(prompts, prompt)
			c := n.Add(1)
			return provider.RunResult{
				FinalText: fmt.Sprintf("%s says %d", name, c),
				TokensIn:  10, TokensOut: 5, ApproxUSDCents: 1,
			}, nil
		},
	}
	if err := deps.Registry.Register(p, false); err != nil {
		t.Fatalf("register %s: %v", name, err)
	}
	return &prompts
}

func discussionReq(rounds int) DiscussionRequest {
	return DiscussionRequest{
		Topic:  "tabs vs spaces",
		Agents: [2]DiscussionAgent{{Provider: "alpha", Persona: "advocate"}, {Provider: "beta", Persona: "skeptic"}},
		Rounds: rounds,
	}
}

func TestDiscuss_HappyPath(t *testing.T) {
	deps := newTestDeps(t)
	alphaPrompts := scriptedAgent(t, deps, "alpha")
	betaPrompts := scriptedAgent(t, deps, "beta")

	res, err := New(deps).Discuss(context.Background(), discussionReq(2))
	if err != nil {
		t.Fatalf("Discuss: %v", err)
	}
	if res.Status != "completed" {
		t.Errorf("status = %q, want completed", res.Status)
	}
	if len(res.Turns) != 4 {
		t.Fatalf("turns = %d, want 4", len(res.Turns))
	}

	// Strict alternation, correct round numbering.
	wantAgents := []string{"alpha", "beta", "alpha", "beta"}
	wantRounds := []int{1, 1, 2, 2}
	for i, turn := range res.Turns {
		if turn.Agent != wantAgents[i] {
			t.Errorf("turn %d agent = %q, want %q", i, turn.Agent, wantAgents[i])
		}
		if turn.Round != wantRounds[i] {
			t.Errorf("turn %d round = %d, want %d", i, turn.Round, wantRounds[i])
		}
	}

	// Transcript threading: beta's first prompt must contain alpha's opening;
	// alpha's rebuttal must contain beta's response.
	if len(*betaPrompts) < 1 || !strings.Contains((*betaPrompts)[0], "alpha says 1") {
		t.Errorf("beta's first prompt missing alpha's opening:\n%s", first(*betaPrompts))
	}
	if len(*alphaPrompts) < 2 || !strings.Contains((*alphaPrompts)[1], "beta says 1") {
		t.Errorf("alpha's rebuttal prompt missing beta's response")
	}
	// Opening prompt has no transcript; rebuttal prompt does.
	if strings.Contains((*alphaPrompts)[0], "Transcript so far") {
		t.Errorf("opening prompt should not embed a transcript")
	}
	// Final-round turns get closing-statement instructions.
	if !strings.Contains((*alphaPrompts)[1], "CLOSING statement") {
		t.Errorf("final-round prompt missing closing instructions:\n%s", (*alphaPrompts)[1])
	}

	// Synthesis defaults to agent A's provider — alpha gets a 3rd call whose
	// prompt is the moderator template over the full transcript.
	if len(*alphaPrompts) != 3 {
		t.Fatalf("alpha calls = %d, want 3 (2 turns + synthesis)", len(*alphaPrompts))
	}
	synthPrompt := (*alphaPrompts)[2]
	if !strings.Contains(synthPrompt, "impartial moderator") || !strings.Contains(synthPrompt, "beta says 2") {
		t.Errorf("synthesis prompt malformed:\n%s", synthPrompt)
	}
	if res.Synthesis == "" {
		t.Errorf("expected non-empty synthesis")
	}

	// Usage accumulated across 5 calls (4 turns + synthesis).
	if res.TokensIn != 50 || res.TokensOut != 25 || res.USDCents != 5 {
		t.Errorf("usage = in:%d out:%d cents:%d, want 50/25/5", res.TokensIn, res.TokensOut, res.USDCents)
	}

	// Persistence: session completed with a final answer ref; one subtask
	// row per turn, ordered, all completed, usage recorded.
	sess, err := deps.Store.GetSession(res.SessionID)
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if sess.Status != "completed" || sess.FinalAnswerRef == "" {
		t.Errorf("session status=%q finalRef=%q", sess.Status, sess.FinalAnswerRef)
	}
	if !strings.Contains(sess.MetaJSON, `"discussion"`) {
		t.Errorf("session meta missing discussion block: %s", sess.MetaJSON)
	}
	subs, err := deps.Store.SubtaskListBySession(res.SessionID, 0, 0)
	if err != nil {
		t.Fatalf("SubtaskListBySession: %v", err)
	}
	if len(subs) != 4 {
		t.Fatalf("subtask rows = %d, want 4", len(subs))
	}
	for i, sub := range subs {
		if sub.Status != "completed" {
			t.Errorf("subtask %d status = %q", i, sub.Status)
		}
		if sub.Ord != i {
			t.Errorf("subtask %d ord = %d", i, sub.Ord)
		}
		if !strings.Contains(sub.MetaJSON, "tokens_in") {
			t.Errorf("subtask %d missing usage meta: %s", i, sub.MetaJSON)
		}
	}
}

func TestDiscuss_SkipSynthesis(t *testing.T) {
	deps := newTestDeps(t)
	alphaPrompts := scriptedAgent(t, deps, "alpha")
	scriptedAgent(t, deps, "beta")

	req := discussionReq(1)
	req.SkipSynthesis = true
	res, err := New(deps).Discuss(context.Background(), req)
	if err != nil {
		t.Fatalf("Discuss: %v", err)
	}
	if res.Synthesis != "" {
		t.Errorf("expected empty synthesis, got %q", res.Synthesis)
	}
	if len(*alphaPrompts) != 1 {
		t.Errorf("alpha calls = %d, want 1 (no synthesis call)", len(*alphaPrompts))
	}
}

func TestDiscuss_Validation(t *testing.T) {
	deps := newTestDeps(t)
	scriptedAgent(t, deps, "alpha")
	sup := New(deps)

	cases := []struct {
		name string
		req  DiscussionRequest
		want string
	}{
		{"empty topic", DiscussionRequest{Agents: [2]DiscussionAgent{{Provider: "alpha"}, {Provider: "alpha"}}}, "topic is empty"},
		{"missing provider", DiscussionRequest{Topic: "t", Agents: [2]DiscussionAgent{{Provider: "alpha"}, {}}}, "provider is required"},
		{"unregistered provider", DiscussionRequest{Topic: "t", Agents: [2]DiscussionAgent{{Provider: "alpha"}, {Provider: "ghost"}}}, "not registered"},
		{"too many rounds", DiscussionRequest{Topic: "t", Rounds: 99, Agents: [2]DiscussionAgent{{Provider: "alpha"}, {Provider: "alpha"}}}, "rounds must be"},
		{"bad synth", DiscussionRequest{Topic: "t", SynthWorker: "ghost", Agents: [2]DiscussionAgent{{Provider: "alpha"}, {Provider: "alpha"}}}, "synthesizer"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res, err := sup.Discuss(context.Background(), tc.req)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want error containing %q, got %v", tc.want, err)
			}
			if res.SessionID != "" {
				t.Errorf("validation failure must not create a session")
			}
		})
	}

	// Validation failures leave no zombie sessions.
	sessions, err := deps.Store.ListSessions(10, 0, "")
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	if len(sessions) != 0 {
		t.Errorf("zombie sessions: %+v", sessions)
	}
}

func TestDiscuss_TurnFailureFailsSession(t *testing.T) {
	deps := newTestDeps(t)
	scriptedAgent(t, deps, "alpha")
	boom := &fakeProvider{
		name: "beta",
		run: func(context.Context, string, provider.RunOptions, chan<- provider.Event) (provider.RunResult, error) {
			return provider.RunResult{}, errors.New("provider exploded")
		},
	}
	if err := deps.Registry.Register(boom, false); err != nil {
		t.Fatalf("register: %v", err)
	}

	res, err := New(deps).Discuss(context.Background(), discussionReq(2))
	if err == nil || !strings.Contains(err.Error(), "provider exploded") {
		t.Fatalf("want turn failure, got %v", err)
	}
	if res.Status != "failed" {
		t.Errorf("status = %q, want failed", res.Status)
	}
	if len(res.Turns) != 1 {
		t.Errorf("completed turns = %d, want 1 (alpha's opening)", len(res.Turns))
	}
	sess, gerr := deps.Store.GetSession(res.SessionID)
	if gerr != nil {
		t.Fatalf("GetSession: %v", gerr)
	}
	if sess.Status != "failed" {
		t.Errorf("session status = %q, want failed", sess.Status)
	}
}

func TestDiscuss_CancelMidDebate(t *testing.T) {
	deps := newTestDeps(t)
	scriptedAgent(t, deps, "alpha")
	ctx, cancel := context.WithCancel(context.Background())
	blocker := &fakeProvider{
		name: "beta",
		run: func(c context.Context, _ string, _ provider.RunOptions, _ chan<- provider.Event) (provider.RunResult, error) {
			cancel() // simulate Ctrl-C while beta's turn is in flight
			<-c.Done()
			return provider.RunResult{}, c.Err()
		},
	}
	if err := deps.Registry.Register(blocker, false); err != nil {
		t.Fatalf("register: %v", err)
	}

	res, err := New(deps).Discuss(ctx, discussionReq(2))
	if err == nil {
		t.Fatal("expected cancellation error")
	}
	if res.Status != "cancelled" {
		t.Errorf("status = %q, want cancelled", res.Status)
	}
	if len(res.Turns) != 1 {
		t.Errorf("turns before cancel = %d, want 1", len(res.Turns))
	}
	sess, gerr := deps.Store.GetSession(res.SessionID)
	if gerr != nil {
		t.Fatalf("GetSession: %v", gerr)
	}
	if sess.Status != "cancelled" {
		t.Errorf("session status = %q, want cancelled", sess.Status)
	}
}

func TestDiscuss_BudgetExhausted(t *testing.T) {
	deps := newTestDeps(t)
	scriptedAgent(t, deps, "alpha") // reports 15 tokens per call
	scriptedAgent(t, deps, "beta")

	req := discussionReq(2)
	req.MaxTokens = 20 // first turn (15) passes; pre-call check trips before turn 3
	res, err := New(deps).Discuss(context.Background(), req)
	if err == nil || !errors.Is(err, budget.ErrBudgetExceeded) {
		t.Fatalf("want ErrBudgetExceeded, got %v", err)
	}
	if res.Status != "budget_exhausted" {
		t.Errorf("status = %q, want budget_exhausted", res.Status)
	}
	sess, gerr := deps.Store.GetSession(res.SessionID)
	if gerr != nil {
		t.Fatalf("GetSession: %v", gerr)
	}
	if sess.Status != "budget_exhausted" {
		t.Errorf("session status = %q, want budget_exhausted", sess.Status)
	}
}

func TestDiscuss_SynthesisFailureKeepsTranscript(t *testing.T) {
	deps := newTestDeps(t)
	scriptedAgent(t, deps, "alpha")
	scriptedAgent(t, deps, "beta")
	badSynth := &fakeProvider{
		name: "moderator",
		run: func(context.Context, string, provider.RunOptions, chan<- provider.Event) (provider.RunResult, error) {
			return provider.RunResult{}, errors.New("moderator down")
		},
	}
	if err := deps.Registry.Register(badSynth, false); err != nil {
		t.Fatalf("register: %v", err)
	}

	req := discussionReq(1)
	req.SynthWorker = "moderator"
	res, err := New(deps).Discuss(context.Background(), req)
	if err != nil {
		t.Fatalf("a failed moderator must not fail the debate: %v", err)
	}
	if res.Status != "completed" {
		t.Errorf("status = %q, want completed", res.Status)
	}
	if res.Synthesis != "" {
		t.Errorf("synthesis should be empty on moderator failure")
	}
	if len(res.Turns) != 2 {
		t.Errorf("turns = %d, want 2", len(res.Turns))
	}
}

func TestDiscuss_EventSequence(t *testing.T) {
	deps := newTestDeps(t)
	scriptedAgent(t, deps, "alpha")
	scriptedAgent(t, deps, "beta")

	ch := deps.Bus.Subscribe(256)
	var kinds []trajectory.Kind
	done := make(chan struct{})
	go func() {
		defer close(done)
		for ev := range ch {
			kinds = append(kinds, ev.Kind)
		}
	}()

	_, err := New(deps).Discuss(context.Background(), discussionReq(1))
	if err != nil {
		t.Fatalf("Discuss: %v", err)
	}
	deps.Bus.Shutdown()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("bus drain timed out")
	}

	count := func(k trajectory.Kind) int {
		n := 0
		for _, kk := range kinds {
			if kk == k {
				n++
			}
		}
		return n
	}
	if count(trajectory.GoalReceived) != 1 {
		t.Errorf("goal_received = %d, want 1", count(trajectory.GoalReceived))
	}
	if count(trajectory.DiscussionTurnStarted) != 2 || count(trajectory.DiscussionTurnDone) != 2 {
		t.Errorf("turn events = %d started / %d done, want 2/2",
			count(trajectory.DiscussionTurnStarted), count(trajectory.DiscussionTurnDone))
	}
	if count(trajectory.DiscussionCompleted) != 1 || count(trajectory.RunCompleted) != 1 {
		t.Errorf("terminal events wrong: %v", kinds)
	}
}

func first(ss []string) string {
	if len(ss) == 0 {
		return "<none>"
	}
	return ss[0]
}
