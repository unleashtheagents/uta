package cli

import (
	"strings"
	"testing"

	"github.com/unleashtheagents/uta/internal/config"
	"github.com/unleashtheagents/uta/internal/engine"
)

func TestDiscussCmd_FlagDefaults(t *testing.T) {
	cmd := newDiscussCmd()
	for flag, want := range map[string]string{
		"rounds":       "3",
		"turn-timeout": "5m0s",
		"timeout":      "30m0s",
		"json":         "false",
		"no-synth":     "false",
	} {
		f := cmd.Flags().Lookup(flag)
		if f == nil {
			t.Fatalf("flag %q not defined", flag)
		}
		if f.DefValue != want {
			t.Errorf("flag %q default = %q, want %q", flag, f.DefValue, want)
		}
	}
}

func TestDiscussCmd_TopicRequired(t *testing.T) {
	cmd := newDiscussCmd()
	cmd.SetArgs([]string{})
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "--topic") {
		t.Fatalf("want topic-required error, got %v", err)
	}
}

func TestResolveDiscussionAgents(t *testing.T) {
	available := []string{"claude", "gemini", "codex"}

	t.Run("default picks first two distinct", func(t *testing.T) {
		got, err := resolveDiscussionAgents(nil, available)
		if err != nil || got != [2]string{"claude", "gemini"} {
			t.Fatalf("got %v, %v", got, err)
		}
	})
	t.Run("explicit pair", func(t *testing.T) {
		got, err := resolveDiscussionAgents([]string{"codex", "claude"}, available)
		if err != nil || got != [2]string{"codex", "claude"} {
			t.Fatalf("got %v, %v", got, err)
		}
	})
	t.Run("same provider twice allowed", func(t *testing.T) {
		got, err := resolveDiscussionAgents([]string{"claude", "claude"}, available)
		if err != nil || got != [2]string{"claude", "claude"} {
			t.Fatalf("got %v, %v", got, err)
		}
	})
	t.Run("wrong count", func(t *testing.T) {
		if _, err := resolveDiscussionAgents([]string{"claude"}, available); err == nil ||
			!strings.Contains(err.Error(), "exactly 2") {
			t.Fatalf("want exactly-2 error, got %v", err)
		}
	})
	t.Run("unavailable provider", func(t *testing.T) {
		if _, err := resolveDiscussionAgents([]string{"claude", "ghost"}, available); err == nil ||
			!strings.Contains(err.Error(), "not available") {
			t.Fatalf("want not-available error, got %v", err)
		}
	})
	t.Run("single available suggests doubling up", func(t *testing.T) {
		_, err := resolveDiscussionAgents(nil, []string{"claude"})
		if err == nil || !strings.Contains(err.Error(), "--agents claude,claude") {
			t.Fatalf("want doubling-up hint, got %v", err)
		}
	})
	t.Run("none available", func(t *testing.T) {
		if _, err := resolveDiscussionAgents(nil, nil); err == nil {
			t.Fatal("want error for zero providers")
		}
	})
}

func TestResolvePersonaText(t *testing.T) {
	personas := []*config.Persona{
		{ID: "skeptic", Prompt: "You doubt everything; demand evidence."},
	}
	if got := resolvePersonaText("skeptic", personas); got != "You doubt everything; demand evidence." {
		t.Errorf("persona id should expand to its prompt, got %q", got)
	}
	if got := resolvePersonaText("free-text role", personas); got != "free-text role" {
		t.Errorf("unknown value should pass through verbatim, got %q", got)
	}
}

func TestDiscussionMarkdown(t *testing.T) {
	res := engine.DiscussionResult{
		SessionID: "sess-1",
		Turns: []engine.DiscussionTurn{
			{Round: 1, AgentIdx: 0, Agent: "claude", Persona: "advocate", Text: "opening"},
			{Round: 1, AgentIdx: 1, Agent: "gemini", Persona: "skeptic", Text: "rebuttal"},
		},
		Synthesis: "the verdict",
	}
	md := discussionMarkdown("tabs vs spaces", res)
	for _, want := range []string{
		"# Debate: tabs vs spaces",
		"## Round 1 — Agent 1: claude (advocate)",
		"## Round 1 — Agent 2: gemini (skeptic)",
		"opening", "rebuttal",
		"# Moderator synthesis",
		"the verdict",
	} {
		if !strings.Contains(md, want) {
			t.Errorf("markdown missing %q:\n%s", want, md)
		}
	}
}
