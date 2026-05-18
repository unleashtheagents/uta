package engine

import (
	"fmt"
	"strings"
)

// SubtaskOutcome is what the supervisor feeds into the synthesis prompt for
// one finished (or failed) subtask.
type SubtaskOutcome struct {
	ID      string
	Title   string
	Worker  string // name of the worker provider that ran (or would have run) the subtask
	Result  string // worker FinalText, or an error/skip stub if the subtask did not complete
	Failed  bool   // the subtask itself ran and errored
	Skipped bool   // the subtask never ran (upstream dep failed, or fail-fast cascade)
}

const synthPromptHeader = `You are the synthesis step of an agent orchestrator called uta.

The user's original goal and the outputs of the parallel subtasks that were
dispatched to address it are below. Produce a single coherent answer that
addresses the original goal, drawing on the subtask outputs as evidence.
Be concrete; if a subtask failed, note the gap but proceed with what's available.

Do not include meta-commentary about the orchestration itself. Write the
final answer as if you authored it yourself.

`

// joinOutcomes formats subtask outcomes as a single Markdown document. Used
// when synthesis is skipped (SkipSynthesis) or when the synthesizer itself
// fails and we fall back to a verbatim join. Including the worker name in
// each header keeps multi-model workflows attributable in the final report.
func joinOutcomes(outcomes []SubtaskOutcome) string {
	var b strings.Builder
	for _, o := range outcomes {
		if o.Worker != "" {
			fmt.Fprintf(&b, "## %s (%s) — worker: %s\n\n%s\n\n", o.Title, o.ID, o.Worker, o.Result)
		} else {
			fmt.Fprintf(&b, "## %s (%s)\n\n%s\n\n", o.Title, o.ID, o.Result)
		}
	}
	return strings.TrimSpace(b.String())
}

// RenderSynthPrompt builds the synthesis prompt.
func RenderSynthPrompt(goal string, outcomes []SubtaskOutcome) string {
	var b strings.Builder
	b.WriteString(synthPromptHeader)
	b.WriteString("Original goal:\n")
	b.WriteString(strings.TrimSpace(goal))
	b.WriteString("\n\nSubtask outputs:\n")
	for _, o := range outcomes {
		marker := "ok"
		switch {
		case o.Skipped:
			marker = "SKIPPED"
		case o.Failed:
			marker = "FAILED"
		}
		fmt.Fprintf(&b, "\n--- [%s] %s (%s) ---\n%s\n", o.ID, o.Title, marker, strings.TrimSpace(o.Result))
	}
	b.WriteString("\n\nNow write the final answer.")
	return b.String()
}
