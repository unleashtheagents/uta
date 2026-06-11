package profileperf

import (
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"time"
)

const (
	barWidth = 30

	// Truncation widths are chosen so the resulting tabwriter lines fit in
	// 80 columns even with the longest realistic fixed-column values
	// (8-char short ids, "completed" status, "1m02s" durations, "builtin"
	// kinds, and MCP tool names up to ~35 chars). See
	// TestRenderSession_80ColMaxWidth for the regression guard.
	maxGoalChars     = 72
	maxTitleChars    = 28
	maxGateCmdChars  = 45
	maxToolNameChars = 36
)

// RenderSession writes the session breakdown as an ascii flame-graph-style
// summary. The layout is intentionally pipeable into a pager — no ANSI
// control sequences, no live refresh.
func RenderSession(w io.Writer, b Breakdown) {
	fmt.Fprintf(w, "session %s  worker=%s  status=%s  mode=%s\n",
		short(b.SessionID), dash(b.Worker), dash(b.Status), dash(b.ModeName))
	if b.Goal != "" {
		fmt.Fprintf(w, "goal: %s\n", truncate(b.Goal, maxGoalChars))
	}
	speed := 0.0
	if b.WallClock > 0 {
		speed = float64(b.CPUEquivalent) / float64(b.WallClock)
	}
	fmt.Fprintf(w, "wall-clock: %s   cpu-equivalent: %s   parallel-speedup: %.2fx\n\n",
		fmtDur(b.WallClock), fmtDur(b.CPUEquivalent), speed)

	type row struct {
		name string
		dur  time.Duration
		n    int
	}
	rows := []row{
		{"planner", b.Planner.Duration, b.Planner.Count},
		{"subtasks", b.SubtasksTotal, len(b.Subtasks)},
		{"synthesis", b.Synthesis.Duration, b.Synthesis.Count},
		{"gates", b.GatesTotal, len(b.Gates)},
		{"external-tools", b.ToolsTotal, len(b.ExternalTools)},
		{"provider-tools", b.ProviderToolTotal, providerToolCount(b)},
	}
	// Share is computed against the largest of (cpu-equivalent, wall-clock)
	// so the bar lengths stay sensible whether the run was serial or
	// heavily parallel.
	denom := b.CPUEquivalent
	if b.WallClock > denom {
		denom = b.WallClock
	}

	fmt.Fprintln(w, "== Phases ==")
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "PHASE\tCOUNT\tDURATION\tSHARE\tBAR")
	for _, r := range rows {
		share := 0.0
		if denom > 0 {
			share = 100 * float64(r.dur) / float64(denom)
		}
		fmt.Fprintf(tw, "%s\t%d\t%s\t%5.1f%%\t%s\n",
			r.name, r.n, fmtDur(r.dur), share, bar(share))
	}
	_ = tw.Flush()
	fmt.Fprintln(w)

	if len(b.Subtasks) > 0 {
		fmt.Fprintln(w, "== Subtasks ==")
		stw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		fmt.Fprintln(stw, "SPEC\tID\tWORKER\tSTATUS\tDURATION\tTITLE")
		for _, s := range b.Subtasks {
			fmt.Fprintf(stw, "%s\t%s\t%s\t%s\t%s\t%s\n",
				dash(s.SpecID), short(s.SubtaskID), dash(s.Worker), dash(s.Status),
				fmtDur(s.Duration), truncate(s.Title, maxTitleChars))
		}
		_ = stw.Flush()
		fmt.Fprintln(w)
	}

	if len(b.Gates) > 0 {
		fmt.Fprintln(w, "== Gate executions ==")
		gtw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		fmt.Fprintln(gtw, "STATUS\tDURATION\tSUBTASK\tCMD")
		for _, g := range b.Gates {
			fmt.Fprintf(gtw, "%s\t%s\t%s\t%s\n",
				dash(g.Status), fmtDur(g.Duration), short(g.SubtaskID),
				truncate(g.Cmd, maxGateCmdChars))
		}
		_ = gtw.Flush()
		fmt.Fprintln(w)
	}

	if len(b.ExternalTools) > 0 {
		fmt.Fprintln(w, "== External tools ==")
		etw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		fmt.Fprintln(etw, "TOOL\tSTATUS\tDURATION\tSUBTASK")
		for _, t := range b.ExternalTools {
			fmt.Fprintf(etw, "%s\t%s\t%s\t%s\n",
				truncate(dash(t.Tool), maxToolNameChars), dash(t.Status), fmtDur(t.Duration), short(t.SubtaskID))
		}
		_ = etw.Flush()
		fmt.Fprintln(w)
	}

	if len(b.ProviderTools) > 0 {
		fmt.Fprintf(w, "== Provider tool calls ==  builtin=%s   mcp=%s\n",
			fmtDur(b.BuiltinToolTotal), fmtDur(b.MCPToolTotal))
		ptw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		fmt.Fprintln(ptw, "TOOL\tKIND\tCALLS\tTOTAL\tAVG")
		for _, t := range b.ProviderTools {
			avg := time.Duration(0)
			if t.Count > 0 {
				avg = t.Duration / time.Duration(t.Count)
			}
			fmt.Fprintf(ptw, "%s\t%s\t%d\t%s\t%s\n",
				truncate(dash(t.Tool), maxToolNameChars), dash(t.Kind), t.Count, fmtDur(t.Duration), fmtDur(avg))
		}
		_ = ptw.Flush()
		fmt.Fprintln(w)
	}

	if b.SentinelAlerts > 0 {
		fmt.Fprintf(w, "sentinel alerts: %d\n", b.SentinelAlerts)
	}
}

// RenderAggregate writes the aggregated cross-session view. Header carries
// the filter (mode + since) the caller used so the report is
// self-explanatory in shell scrollback.
func RenderAggregate(w io.Writer, agg Aggregate, modeName string, since time.Time) {
	header := fmt.Sprintf("aggregate across %d session(s)", agg.Sessions)
	if modeName != "" {
		header += "  mode=" + modeName
	}
	if !since.IsZero() {
		header += "  since=" + since.UTC().Format(time.RFC3339)
	}
	fmt.Fprintln(w, header)
	speed := 0.0
	if agg.WallClock > 0 {
		speed = float64(agg.CPUEquivalent) / float64(agg.WallClock)
	}
	fmt.Fprintf(w, "wall-clock (sum): %s   cpu-equivalent: %s   avg-speedup: %.2fx\n\n",
		fmtDur(agg.WallClock), fmtDur(agg.CPUEquivalent), speed)

	type row struct {
		name string
		dur  time.Duration
	}
	rows := []row{
		{"planner", agg.PlannerTime},
		{"subtasks", agg.SubtasksTime},
		{"synthesis", agg.SynthesisTime},
		{"gates", agg.GatesTime},
		{"external-tools", agg.ExternalToolsTime},
		{"provider-tools", agg.ProviderToolTime},
	}
	denom := agg.CPUEquivalent
	if denom == 0 {
		denom = agg.WallClock
	}
	fmt.Fprintln(w, "== Phases (sums) ==")
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "PHASE\tTOTAL\tAVG/SESSION\tSHARE\tBAR")
	for _, r := range rows {
		share := 0.0
		if denom > 0 {
			share = 100 * float64(r.dur) / float64(denom)
		}
		avg := time.Duration(0)
		if agg.Sessions > 0 {
			avg = r.dur / time.Duration(agg.Sessions)
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%5.1f%%\t%s\n",
			r.name, fmtDur(r.dur), fmtDur(avg), share, bar(share))
	}
	_ = tw.Flush()
	fmt.Fprintln(w)

	if len(agg.TopTools) > 0 {
		fmt.Fprintf(w, "== Top provider tools ==  builtin=%s   mcp=%s\n",
			fmtDur(agg.BuiltinToolTime), fmtDur(agg.MCPToolTime))
		ttw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		fmt.Fprintln(ttw, "TOOL\tKIND\tCALLS\tTOTAL\tAVG")
		for _, t := range agg.TopTools {
			avg := time.Duration(0)
			if t.Count > 0 {
				avg = t.Duration / time.Duration(t.Count)
			}
			fmt.Fprintf(ttw, "%s\t%s\t%d\t%s\t%s\n",
				truncate(dash(t.Tool), maxToolNameChars), dash(t.Kind), t.Count, fmtDur(t.Duration), fmtDur(avg))
		}
		_ = ttw.Flush()
		fmt.Fprintln(w)
	}

	if agg.SentinelAlerts > 0 {
		fmt.Fprintf(w, "sentinel alerts (sum): %d\n", agg.SentinelAlerts)
	}
}

func bar(share float64) string {
	if share <= 0 {
		return ""
	}
	n := int(share/100*float64(barWidth) + 0.5)
	if n < 1 {
		n = 1
	}
	if n > barWidth {
		n = barWidth
	}
	return strings.Repeat("#", n)
}

func fmtDur(d time.Duration) string {
	if d <= 0 {
		return "0s"
	}
	if d < time.Millisecond {
		return d.String()
	}
	if d < time.Second {
		return fmt.Sprintf("%dms", d/time.Millisecond)
	}
	if d < time.Minute {
		secs := float64(d) / float64(time.Second)
		return fmt.Sprintf("%.1fs", secs)
	}
	if d < time.Hour {
		m := int(d / time.Minute)
		s := int((d % time.Minute) / time.Second)
		return fmt.Sprintf("%dm%02ds", m, s)
	}
	h := int(d / time.Hour)
	m := int((d % time.Hour) / time.Minute)
	return fmt.Sprintf("%dh%02dm", h, m)
}

func short(s string) string {
	if len(s) >= 8 {
		return s[:8]
	}
	if s == "" {
		return "-"
	}
	return s
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func truncate(s string, max int) string {
	s = strings.ReplaceAll(strings.ReplaceAll(s, "\r", " "), "\n", " ")
	if len(s) <= max {
		return s
	}
	return s[:max-1] + "…"
}

func providerToolCount(b Breakdown) int {
	n := 0
	for _, t := range b.ProviderTools {
		n += t.Count
	}
	return n
}
