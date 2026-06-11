package engine

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/unleashtheagents/uta/internal/trajectory"
)

// whiteboardMaxEntries caps how many distinct keys are rendered into the
// planner prompt's "Shared whiteboard" block. The whiteboard is meant for
// short cross-mode notes (a blocker, a handoff hint, a flag), not for
// dumping arbitrary state — a small ceiling keeps the prompt budget
// predictable.
const whiteboardMaxEntries = 10

// whiteboardBlock renders the planner-prompt "## Shared whiteboard" section,
// or returns an empty string when there are no entries or the whiteboard
// dep is not wired. Each entry is one line: key, latest writer (mode), and
// a one-line preview of the JSON value. Authors are surfaced so a dev-mode
// planner can recognize "ops left this for me" at a glance.
//
// Emits one WhiteboardGet trajectory event per call that returns content,
// so observability surfaces can attribute cross-mode reads.
func (s *Supervisor) whiteboardBlock(sessionID string) string {
	if s.deps.Whiteboard == nil {
		return ""
	}
	entries, err := s.deps.Whiteboard.List(whiteboardMaxEntries)
	if err != nil {
		s.dbErr(sessionID, "", "whiteboard_list", err)
		return ""
	}
	if len(entries) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("## Shared whiteboard\n\n")
	b.WriteString("Notes other modes have left on the shared whiteboard. Take them into account when planning:\n\n")
	keys := make([]string, 0, len(entries))
	for _, e := range entries {
		author := e.AuthorMode
		if author == "" {
			author = "no-mode"
		}
		b.WriteString(fmt.Sprintf("- [%s · %s] %s\n", e.Key, author, oneLineWhiteboardValue(e.ValueJSON)))
		keys = append(keys, e.Key)
	}
	b.WriteString("\n")
	s.emit(sessionID, "", trajectory.WhiteboardGet, map[string]any{
		"count":  len(entries),
		"keys":   keys,
		"source": "planner_inject",
	})
	return b.String()
}

// observeWhiteboardToolCall emits a semantic WhiteboardSet / WhiteboardGet
// trajectory event alongside the generic SubtaskToolCall when a worker
// invoked one of the uta_whiteboard_* MCP tools. The parallel emit lets
// observability surfaces (retrospectives, dashboards) filter on
// whiteboard activity without parsing tool_call payloads, and gives the
// WhiteboardSet kind a real producer instead of being a dead enum value.
//
// No-op when the tool name doesn't match one of the whiteboard tools, so
// callers can hand every tool_call event through.
func (s *Supervisor) observeWhiteboardToolCall(sessionID, subtaskID, toolName string, input json.RawMessage) {
	switch toolName {
	case "uta_whiteboard_set":
		s.emit(sessionID, subtaskID, trajectory.WhiteboardSet, map[string]any{
			"source": "tool_call",
			"tool":   toolName,
			"input":  input,
		})
	case "uta_whiteboard_get", "uta_whiteboard_list":
		s.emit(sessionID, subtaskID, trajectory.WhiteboardGet, map[string]any{
			"source": "tool_call",
			"tool":   toolName,
			"input":  input,
		})
	}
}

// oneLineWhiteboardValue collapses a JSON value into a single planner-
// prompt-friendly line. The cap mirrors oneLineBody in memory_hooks so
// the two injected blocks have the same per-entry budget.
func oneLineWhiteboardValue(v string) string {
	first := strings.TrimSpace(v)
	first = strings.ReplaceAll(first, "\n", " ")
	first = strings.ReplaceAll(first, "\r", " ")
	const max = 240
	if len(first) > max {
		first = first[:max] + "…"
	}
	return first
}
