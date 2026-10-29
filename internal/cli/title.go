package cli

import (
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

// titleGoal shortens a goal for use in a terminal title.
func titleGoal(goal string) string {
	goal = strings.Join(strings.Fields(goal), " ")
	if len(goal) > 60 {
		goal = goal[:57] + "..."
	}
	return goal
}
