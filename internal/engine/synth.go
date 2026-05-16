package engine

import (
	"fmt"
	"strings"
)

// SubtaskOutcome is what the supervisor feeds into the synthesis prompt for
// one finished (or failed) subtask.
type SubtaskOutcome struct {
	ID     string
	Title  string
	Result string // worker FinalText, or an error stub if the subtask failed
	Failed bool
}

const synthPromptHeader = `You are the synthesis step of an agent orchestrator called uta.

The user's original goal and the outputs of the parallel subtasks that were
dispatched to address it are below. Produce a single coherent answer that
addresses the original goal, drawing on the subtask outputs as evidence.
Be concrete; if a subtask failed, note the gap but proceed with what's available.

Do not include meta-commentary about the orchestration itself. Write the
final answer as if you authored it yourself.

`

// RenderSynthPrompt builds the synthesis prompt.
func RenderSynthPrompt(goal string, outcomes []SubtaskOutcome) string {
	var b strings.Builder
	b.WriteString(synthPromptHeader)
	b.WriteString("Original goal:\n")
	b.WriteString(strings.TrimSpace(goal))
	b.WriteString("\n\nSubtask outputs:\n")
	for _, o := range outcomes {
		marker := "ok"
		if o.Failed {
			marker = "FAILED"
		}
		fmt.Fprintf(&b, "\n--- [%s] %s (%s) ---\n%s\n", o.ID, o.Title, marker, strings.TrimSpace(o.Result))
	}
	b.WriteString("\n\nNow write the final answer.")
	return b.String()
}
