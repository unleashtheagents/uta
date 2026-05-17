package improve

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/unleashtheagents/uta/internal/provider"
)

// Gatherer asks a worker (provider) to read a codebase and propose
// improvement ideas. Returns parsed ideas; the caller persists them via
// Board.Insert.
type Gatherer struct {
	Registry *provider.Registry
}

// GatherRequest configures one gather call.
type GatherRequest struct {
	WorkerName string         // provider to invoke; defaults to "gemini" because of its context-window advantage
	Goal       string         // optional extra steering ("focus on test coverage", etc.)
	Workdir    string         // exposed to the worker
	Env        []string       // extra env (e.g. UTA_PROJECT_ROOT)
	Timeout    time.Duration  // per-call timeout; default 10m
	MaxIdeas   int            // hard cap on the number of ideas returned; default 15
	Tags       []string       // tags applied to every produced idea
	Existing   []*Idea        // already-known ideas; the gatherer is told to avoid duplicates
}

// GatherResult is what the gatherer returns. Ideas are NOT yet persisted —
// the caller decides what to do with them.
type GatherResult struct {
	WorkerName  string
	Ideas       []*Idea
	RawResponse string
	SessionID   string // provider's own session id, if any
}

const gatherPrompt = `You are the idea-gathering step of the uta self-improvement loop.

Read the codebase under %s carefully (use your file-inspection tools) and
propose concrete, actionable improvements. Each idea should be small enough
that a single agent can implement it in one pass with passing tests at the end.

%sAvoid duplicates with the existing backlog below. Be specific — "improve
performance" is not an idea; "cache parsed YAML descriptors in
internal/provider/registry.go to avoid re-parsing on every Detect() call" is.

Severity scale:
- HIGH = a bug, security issue, or clear correctness problem
- MEDIUM = a meaningful refactor or feature that increases robustness
- LOW = a polish item (naming, doc, small cleanup)
- INFO = an observation that may or may not be worth acting on

Return ONLY a single JSON object — no prose, no markdown, no code fences:

{
  "ideas": [
    {
      "title": "<one-line summary>",
      "body":  "<2–6 sentences: what to change, why, and the rough how>",
      "severity": "HIGH" | "MEDIUM" | "LOW" | "INFO",
      "tags": ["optional", "free-form"]
    }
  ]
}

Hard limits:
- propose AT MOST %d ideas, sorted by severity then importance
- never invent files or symbols that don't exist
- do not propose anything that requires a deploy, push, or destructive op

Existing ideas already on the backlog (DO NOT repropose):
%s
`

// Run executes one gather call against the chosen worker.
func (g *Gatherer) Run(ctx context.Context, req GatherRequest) (*GatherResult, error) {
	if g.Registry == nil {
		return nil, errors.New("gatherer: registry is nil")
	}
	if req.WorkerName == "" {
		req.WorkerName = "gemini"
	}
	prov, ok := g.Registry.Get(req.WorkerName)
	if !ok {
		return nil, fmt.Errorf("gatherer: worker %q not registered", req.WorkerName)
	}
	if req.MaxIdeas <= 0 {
		req.MaxIdeas = 15
	}
	if req.Timeout <= 0 {
		req.Timeout = 10 * time.Minute
	}
	workdir := req.Workdir
	if workdir == "" {
		workdir = "."
	}
	extraGoal := ""
	if strings.TrimSpace(req.Goal) != "" {
		extraGoal = "Specific focus from the operator: " + strings.TrimSpace(req.Goal) + "\n\n"
	}
	prompt := fmt.Sprintf(gatherPrompt, workdir, extraGoal, req.MaxIdeas, renderExisting(req.Existing))

	subCtx, cancel := context.WithTimeout(ctx, req.Timeout)
	defer cancel()

	events := make(chan provider.Event, 64)
	go func() {
		for range events {
		}
	}() // drain — caller doesn't subscribe; bus integration is a v0.6.x stretch

	result, err := prov.RunHeadless(subCtx, prompt, provider.RunOptions{
		Workdir: workdir,
		Env:     req.Env,
		Timeout: req.Timeout,
	}, events)
	close(events)
	if err != nil {
		return nil, fmt.Errorf("gatherer call: %w", err)
	}

	ideas, parseErr := parseIdeas(result.FinalText, req.WorkerName, req.Tags)
	if parseErr != nil {
		return &GatherResult{
			WorkerName:  req.WorkerName,
			RawResponse: result.FinalText,
			SessionID:   result.SessionID,
		}, parseErr
	}
	return &GatherResult{
		WorkerName:  req.WorkerName,
		Ideas:       ideas,
		RawResponse: result.FinalText,
		SessionID:   result.SessionID,
	}, nil
}

// renderExisting builds a compact listing of titles to feed to the prompt.
// We deliberately keep it terse — the gatherer just needs to know what's on
// the board, not the full body of each prior idea.
func renderExisting(existing []*Idea) string {
	if len(existing) == 0 {
		return "(backlog is empty)"
	}
	var b strings.Builder
	for _, idea := range existing {
		fmt.Fprintf(&b, "- [%s · %s] %s\n", idea.Severity, idea.Status, idea.Title)
	}
	return b.String()
}

// parseIdeas is the permissive JSON extractor: locates the first '{' through
// last '}', json.Unmarshal's, normalizes severity, drops empties.
func parseIdeas(text, source string, tags []string) ([]*Idea, error) {
	start := strings.Index(text, "{")
	end := strings.LastIndex(text, "}")
	if start < 0 || end <= start {
		return nil, errors.New("gatherer: no JSON object in response")
	}
	var doc struct {
		Ideas []struct {
			Title    string   `json:"title"`
			Body     string   `json:"body"`
			Severity string   `json:"severity"`
			Tags     []string `json:"tags"`
		} `json:"ideas"`
	}
	if err := json.Unmarshal([]byte(text[start:end+1]), &doc); err != nil {
		return nil, fmt.Errorf("gatherer: parse: %w", err)
	}
	out := make([]*Idea, 0, len(doc.Ideas))
	for _, in := range doc.Ideas {
		title := strings.TrimSpace(in.Title)
		body := strings.TrimSpace(in.Body)
		if title == "" && body == "" {
			continue
		}
		sev := normalizeSeverity(in.Severity)
		combined := append([]string{}, tags...)
		for _, t := range in.Tags {
			t = strings.TrimSpace(t)
			if t != "" {
				combined = append(combined, t)
			}
		}
		out = append(out, &Idea{
			Title:    title,
			Body:     body,
			Source:   "gather:" + source,
			Severity: sev,
			Status:   StatusProposed,
			Tags:     combined,
		})
	}
	return out, nil
}

func normalizeSeverity(s string) Severity {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "high", "critical", "crit", "h":
		return SevHigh
	case "medium", "med", "moderate", "m":
		return SevMedium
	case "low", "minor", "l":
		return SevLow
	case "info", "informational", "note", "nit", "i":
		return SevInfo
	}
	return SevMedium
}
