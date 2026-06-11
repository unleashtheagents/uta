package engine

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/unleashtheagents/uta/internal/memory"
	"github.com/unleashtheagents/uta/internal/trajectory"
)

// defaultMemoryTopK is the number of relevant prior facts the planner
// gets when a profile opts in to memory injection without overriding
// MemoryTopK. Three balances "useful context" against "don't bury the
// goal under stale notes."
const defaultMemoryTopK = 3

// writeHighFindingsToMemory persists every HIGH-severity finding in the
// supplied report as a lint_rule fact. No-op when the supervisor has no
// memory dep wired up or the report has nothing severe enough to warrant
// remembering. Errors are recorded on the trajectory bus rather than
// returned — memory writes are best-effort and must not derail a run.
func (s *Supervisor) writeHighFindingsToMemory(sessionID, modeName string, report *FindingsReport) {
	if s.deps.Memory == nil || report == nil {
		return
	}
	for i := range report.Findings {
		f := &report.Findings[i]
		if f.Severity != SevHigh {
			continue
		}
		body := strings.TrimSpace(f.Body)
		if body == "" {
			body = f.Title
		}
		fact := memory.Fact{
			SourceMode:      modeName,
			SourceSessionID: sessionID,
			Kind:            "lint_rule",
			Body:            fmt.Sprintf("%s\n\n%s", strings.TrimSpace(f.Title), body),
			Tags:            findingTags(f, modeName),
		}
		id, err := s.deps.Memory.Write(fact)
		if err != nil {
			s.dbErr(sessionID, "", "memory_write_lint_rule", err)
			continue
		}
		s.emit(sessionID, "", trajectory.MemoryFactWritten, map[string]any{
			"id":     id,
			"kind":   fact.Kind,
			"source": "high_finding",
			"tags":   fact.Tags,
		})
	}
}

// consolidateAuditOutcome writes a single session_outcome fact summarizing
// the audit run, when the profile opted in via memory.consolidate. The
// body is deterministic (goal + status + finding counts) so this v1
// hook works without an LLM summarization pass — that's reserved for a
// later iteration per the idea's out-of-scope note.
func (s *Supervisor) consolidateAuditOutcome(sessionID string, req ReflectorRequest, res *ReflectorResult) {
	if s.deps.Memory == nil {
		return
	}
	var stats map[string]int
	highest := SevUnknown
	if res.FinalFindings != nil {
		stats = res.FinalFindings.Stats
		highest = res.FinalFindings.HighestSeverity()
	}
	body := fmt.Sprintf(
		"Audit goal: %s\nStatus: %s\nIterations: %d\nStopped because: %s\nHighest severity: %s\nFinding counts: %v",
		strings.TrimSpace(req.Goal), res.Status, res.Iterations, res.StoppedBecause, string(highest), stats,
	)
	fact := memory.Fact{
		SourceMode:      req.ModeName,
		SourceSessionID: sessionID,
		Kind:            "session_outcome",
		Body:            body,
		Tags:            consolidateTagsFromGoal(req.Goal, req.ModeName),
	}
	id, err := s.deps.Memory.Write(fact)
	if err != nil {
		s.dbErr(sessionID, "", "memory_write_session_outcome", err)
		return
	}
	s.emit(sessionID, "", trajectory.MemoryFactWritten, map[string]any{
		"id":     id,
		"kind":   fact.Kind,
		"source": "consolidate",
	})
}

// consolidateRunOutcome is the supervisor.Run-side counterpart to
// consolidateAuditOutcome. Writes a single session_outcome fact from the
// final RunResult when the active profile opted in via memory.consolidate.
func (s *Supervisor) consolidateRunOutcome(sessionID string, req RunRequest, res *RunResult) {
	if s.deps.Memory == nil {
		return
	}
	body := fmt.Sprintf(
		"Goal: %s\nStatus: %s\nSubtasks: %d",
		strings.TrimSpace(req.Goal), res.Status, len(res.Subtasks),
	)
	fact := memory.Fact{
		SourceMode:      req.ModeName,
		SourceSessionID: sessionID,
		Kind:            "session_outcome",
		Body:            body,
		Tags:            consolidateTagsFromGoal(req.Goal, req.ModeName),
	}
	id, err := s.deps.Memory.Write(fact)
	if err != nil {
		s.dbErr(sessionID, "", "memory_write_session_outcome", err)
		return
	}
	s.emit(sessionID, "", trajectory.MemoryFactWritten, map[string]any{
		"id":     id,
		"kind":   fact.Kind,
		"source": "consolidate",
	})
}

// memorySearchTimeout caps the hybrid search (which may include one
// embedding API round trip for the query vector). Planning-latency
// budget, not correctness — on timeout the tag-overlap fallback runs.
const memorySearchTimeout = 15 * time.Second

// relevantMemoryBlock returns the planner-prompt "## Relevant prior
// facts" section for the supplied goal, or an empty string when there's
// nothing to inject.
//
// Retrieval is two-stage: the hybrid RAG index (FTS5/BM25 + vector KNN
// when an embedder is configured — see memory/rag.go) is tried first;
// when it yields nothing (e.g. a database from before the RAG migration
// that was never reindexed) the v1 tag-overlap ranking is the fallback,
// so memory injection never regresses.
//
// The returned block ends with a trailing blank line so callers can
// concatenate it verbatim before the goal section.
func (s *Supervisor) relevantMemoryBlock(sessionID, goal string, topK int) string {
	if s.deps.Memory == nil {
		return ""
	}
	if topK < 0 {
		return ""
	}
	if topK == 0 {
		topK = defaultMemoryTopK
	}

	retrieval := "rag"
	var lines []string
	ctx, cancel := context.WithTimeout(context.Background(), memorySearchTimeout)
	defer cancel()
	results, err := s.deps.Memory.SearchHybrid(ctx, goal, topK)
	if err != nil {
		s.dbErr(sessionID, "", "memory_search_hybrid", err)
	}
	for _, r := range results {
		src := r.ModeName
		if src == "" {
			src = "no-mode"
		}
		lines = append(lines, fmt.Sprintf("- [%s · %s] %s", src, r.Kind, oneLineBody(r.Body)))
	}

	if len(lines) == 0 {
		retrieval = "tag-overlap"
		tokens := memory.TokenizeGoal(goal)
		if len(tokens) == 0 {
			return ""
		}
		facts, err := s.deps.Memory.TopRelevant(tokens, topK)
		if err != nil {
			s.dbErr(sessionID, "", "memory_top_relevant", err)
			return ""
		}
		for _, f := range facts {
			src := f.SourceMode
			if src == "" {
				src = "no-mode"
			}
			lines = append(lines, fmt.Sprintf("- [%s · %s] %s", src, f.Kind, oneLineBody(f.Body)))
		}
	}
	if len(lines) == 0 {
		return ""
	}

	var b strings.Builder
	b.WriteString("## Relevant prior facts\n\n")
	for _, l := range lines {
		b.WriteString(l)
		b.WriteString("\n")
	}
	b.WriteString("\n")
	s.emit(sessionID, "", trajectory.MemoryFactsInjected, map[string]any{
		"count":     len(lines),
		"top_k":     topK,
		"retrieval": retrieval,
	})
	return b.String()
}

// findingTags builds the tag set we attach to a HIGH-finding fact. Tags
// come from: the source mode, file extension, file basename, and the
// finding title (tokenized). Together these are what a future planner's
// tag-overlap query is most likely to hit on.
func findingTags(f *Finding, modeName string) []string {
	tags := []string{string(f.Severity), "finding", f.Critic, modeName}
	if f.File != "" {
		ext := strings.TrimPrefix(strings.ToLower(filepath.Ext(f.File)), ".")
		if ext != "" {
			tags = append(tags, ext)
		}
		base := filepath.Base(f.File)
		tags = append(tags, base)
	}
	tags = append(tags, f.Title)
	return tags
}

// consolidateTagsFromGoal mirrors findingTags but for a goal-level
// session_outcome fact. Mode + tokenized goal text.
func consolidateTagsFromGoal(goal, modeName string) []string {
	tags := []string{modeName, "session_outcome"}
	tags = append(tags, goal)
	return tags
}

// oneLineBody collapses a multi-line fact body into a single planner-
// prompt-friendly line, trimming to a stable max length so a verbose
// fact can't blow out the prompt budget.
func oneLineBody(body string) string {
	first := strings.TrimSpace(body)
	if idx := strings.IndexByte(first, '\n'); idx > 0 {
		first = strings.TrimSpace(first[:idx])
	}
	const max = 240
	if len(first) > max {
		first = first[:max] + "…"
	}
	return first
}
