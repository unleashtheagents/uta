package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/mattn/go-isatty"
)

// setTerminalTitle names the tab/window in terminals that honor OSC 0
// (Warp, iTerm2, Terminal.app, Windows Terminal, most others). It is a
// no-op when the writer is not an interactive terminal, so piped and CI
// runs stay byte-clean. Terminals without a branded icon for uta still
// show the title, which is the best self-identification a CLI can emit.
func setTerminalTitle(w io.Writer, title string) {
	f, ok := w.(*os.File)
	if !ok || !isatty.IsTerminal(f.Fd()) {
		return
	}
	if os.Getenv("TERM") == "dumb" {
		return
	}
	title = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, title)
	if len(title) > 120 {
		title = title[:120]
	}
	fmt.Fprintf(f, "\x1b]0;%s\x07", title)
}

// warpAgentEvent matches Warp Terminal's OSC 777 `warp://cli-agent` v1 schema
// (see warpdotdev/warp `crates/warp_core/src/cli_agent_protocol.rs`).
type warpAgentEvent struct {
	V         int    `json:"v"`
	Agent     string `json:"agent"`
	Event     string `json:"event"`
	SessionID string `json:"session_id,omitempty"`
	Cwd       string `json:"cwd,omitempty"`
	Query     string `json:"query,omitempty"`
	Response  string `json:"response,omitempty"`
}

// formatWarpAgentOSC777 formats a structured Warp CLI-agent notification frame.
func formatWarpAgentOSC777(ev warpAgentEvent) string {
	ev.V = 1
	ev.Agent = "uta"
	if len(ev.Query) > 500 {
		ev.Query = ev.Query[:500]
	}
	if len(ev.Response) > 500 {
		ev.Response = ev.Response[:500]
	}
	b, err := json.Marshal(ev)
	if err != nil {
		return ""
	}
	return fmt.Sprintf("\x1b]777;warp://cli-agent;%s\x07", string(b))
}

// emitWarpAgentEvent emits a structured OSC 777 notification when running
// interactively inside Warp Terminal (TERM_PROGRAM=WarpTerminal). It is a
// no-op everywhere else so non-Warp terminals, pipes, and CI stay byte-clean.
func emitWarpAgentEvent(w io.Writer, ev warpAgentEvent) {
	f, ok := w.(*os.File)
	if !ok || !isatty.IsTerminal(f.Fd()) {
		return
	}
	if os.Getenv("TERM_PROGRAM") != "WarpTerminal" {
		return
	}
	if seq := formatWarpAgentOSC777(ev); seq != "" {
		fmt.Fprint(f, seq)
	}
}

// titleGoal shortens a goal for use in a terminal title.
func titleGoal(goal string) string {
	goal = strings.Join(strings.Fields(goal), " ")
	if len(goal) > 60 {
		goal = goal[:57] + "..."
	}
	return goal
}
