package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/unleashtheagents/uta/internal/engine"
	"github.com/unleashtheagents/uta/internal/improve"
	"github.com/unleashtheagents/uta/internal/orgstate"
	"github.com/unleashtheagents/uta/internal/profile"
	"github.com/unleashtheagents/uta/internal/researchdb"
	"github.com/unleashtheagents/uta/internal/state"
	"github.com/unleashtheagents/uta/internal/store"
)

// paneSpec bundles the two operations `uta dash` needs to perform on a
// per-mode pane: render it as text, and materialize it as a JSON value.
// Keeping the pair together makes paneRegistry the single source of truth
// — adding a new mode means adding one entry here and nothing else (no
// dashJSON struct field, no switch case in emitDashJSON to remember).
type paneSpec struct {
	render func(out io.Writer, d *dashData, tagKey string)
	build  func(d *dashData, tagKey string) any
}

// paneRegistry is the registry of per-mode panes `uta dash` knows about.
// Adding a new mode means registering its renderer+builder here; the
// dash will pick it up automatically the moment a profile or session
// references the name. The LIST of panes shown in the default text view
// is derived from this registry intersected with loaded profiles and
// observed sessions — so a project that has no profile for "research"
// and no research sessions won't print an empty Research pane. The JSON
// snapshot, by contrast, always emits every registered pane (stable
// schema for tooling, keyed by mode name).
var paneRegistry = map[string]paneSpec{
	"dev": {
		render: func(out io.Writer, d *dashData, tagKey string) { renderDashDev(out, d, tagKey) },
		build:  func(d *dashData, tagKey string) any { return buildDevPane(d, tagKey) },
	},
	"ops": {
		render: func(out io.Writer, d *dashData, _ string) { renderDashOps(out, d) },
		build:  func(d *dashData, _ string) any { return buildOpsPane(d) },
	},
	"audit": {
		render: func(out io.Writer, d *dashData, _ string) { renderDashAudit(out, d) },
		build:  func(d *dashData, _ string) any { return buildAuditPane(d) },
	},
	"research": {
		render: func(out io.Writer, d *dashData, _ string) { renderDashResearch(out, d) },
		build:  func(d *dashData, _ string) any { return buildResearchPane(d) },
	},
}

// registeredPaneNames returns every registered pane name, sorted, for
// stable flag-error messages and JSON-snapshot iteration.
func registeredPaneNames() []string {
	names := make([]string, 0, len(paneRegistry))
	for name := range paneRegistry {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// panesToRender selects which per-mode panes the default text view
// should print. A pane is included whenever a profile with that name
// was loaded OR a session recorded that mode. Modes with a specialized
// renderer in paneRegistry get the specialized pane; modes without one
// get the generic ideas-and-sessions pane via the fallback. The result
// is sorted so the rendering order is deterministic across runs.
//
// This is the "generic-first" fix to STATUS-AGENTIC-OS.md Theme C:
// previously only modes in paneRegistry (dev/ops/audit/research) got a
// pane, so user-defined profiles like "growth" or "publishing" were
// silently invisible on the dash even when they had data.
func panesToRender(profiles []*profile.MissionProfile, stats map[string]*store.ModeStat) []string {
	seen := map[string]bool{}
	for _, p := range profiles {
		if p == nil || p.Name == "" {
			continue
		}
		seen[p.Name] = true
	}
	for name, ms := range stats {
		if name == "" || ms == nil {
			continue
		}
		seen[name] = true
	}
	out := make([]string, 0, len(seen))
	for name := range seen {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// newDashCmd assembles the `uta dash` command — a single-render ASCII
// dashboard scriptable enough to pipe into pagers. Intentionally NOT a
// TUI loop: re-running the command is the refresh mechanism.
func newDashCmd() *cobra.Command {
	var (
		recentLimit int
		alertLimit  int
		ideaTagKey  string
		modeFilter  string
		asJSON      bool
	)
	cmd := &cobra.Command{
		Use:   "dash",
		Short: "single-render ASCII dashboard (threads, sessions, alerts, ideas, per-mode panes)",
		Long: `Render a snapshot of project state to stdout. Default sections, in order:

  1. Active threads          — every state.json thread, * marks active
  2. Recent sessions per mode — newest sessions grouped by MissionProfile
  3. Sentinel alerts (24h)   — every sentinel_alert event in the last 24h
  4. Idea board per mode      — counts grouped by '<key>:<name>' tag prefix
  5. Per-mode panes           — one deep-dive per mode for which a renderer
                                is registered AND a profile or session
                                exists. Modes that have neither a profile
                                nor recorded data are skipped.

` + "`uta dash --mode <name>`" + ` renders only the named pane (one of the
registered renderers — see --mode help for the current set).
` + "`uta dash --json`" + ` emits a machine-readable snapshot suitable for
status-bar widgets or other tooling; the JSON schema includes every
registered pane regardless of data state.

` + "`uta dash`" + ` is a single render — it prints once and exits. Run it again
to refresh; a live TUI is intentionally out of scope.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			app, err := newApp(cmd.Context())
			if err != nil {
				return err
			}
			defer app.Close()

			out := cmd.OutOrStdout()
			errOut := cmd.ErrOrStderr()

			modeFilter = strings.ToLower(strings.TrimSpace(modeFilter))
			// Any mode name is accepted: known ones (paneRegistry) get the
			// specialized pane; unknown ones get the generic pane. The
			// behavior is consistent whether the operator names a profile
			// "dev" or "growth" or "publishing" — generic-first.

			data := collectDashData(app, errOut, recentLimit, alertLimit)

			if asJSON {
				return emitDashJSON(out, data, modeFilter, ideaTagKey)
			}

			if modeFilter != "" {
				renderDashPane(out, data, modeFilter, ideaTagKey)
				return nil
			}

			renderDashThreads(out, errOut, app)
			fmt.Fprintln(out)
			renderDashSessions(out, data.Profiles, data.Stats, recentLimit)
			fmt.Fprintln(out)
			renderDashAlerts(out, data.Alerts)
			fmt.Fprintln(out)
			renderDashCost(out, data.CostBuckets24h)
			fmt.Fprintln(out)
			renderDashIdeas(out, data.Ideas, ideaTagKey)
			fmt.Fprintln(out)
			renderDashRetrospectives(out, data.Retros, app.InProject())
			fmt.Fprintln(out)

			for _, name := range panesToRender(data.Profiles, data.Stats) {
				renderDashPane(out, data, name, ideaTagKey)
				fmt.Fprintln(out)
			}
			return nil
		},
	}
	cmd.Flags().IntVar(&recentLimit, "recent", 5, "recent sessions to show per mode")
	cmd.Flags().IntVar(&alertLimit, "alerts", 20, "max sentinel alerts to list")
	cmd.Flags().StringVar(&ideaTagKey, "idea-tag-key", "mode", "tag prefix used to group ideas (e.g. 'mode' matches 'mode:dev')")
	cmd.Flags().StringVar(&modeFilter, "mode", "", "render only the pane for the named mode; any mode name is accepted (specialized panes exist for "+strings.Join(registeredPaneNames(), ", ")+", others get a generic ideas+sessions view)")
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit JSON (full snapshot, or just the pane when --mode is set)")
	return cmd
}

// dashData bundles every cross-section read that the dashboard needs so
// the renderers stay free of I/O. Errors are logged inline to errOut and
// fields are filled in best-effort — a missing slice or stat is treated
// as an empty result, never a fatal failure.
type dashData struct {
	App      *App
	Profiles []*profile.MissionProfile
	Stats    map[string]*store.ModeStat
	Alerts   []store.SentinelAlertRow
	Ideas    []*improve.Idea
	Retros   []improve.RetrospectiveFile
	// CostBuckets24h is the same shape `uta perf --cost --since 24h` emits,
	// summarized to a compact dashboard pane. Empty when no subtask has
	// completed with usage in the window.
	CostBuckets24h []store.CostBucket
}

func collectDashData(app *App, errOut io.Writer, recentLimit, alertLimit int) *dashData {
	d := &dashData{App: app}
	profiles, perrs := profile.LoadAll(app.GlobalHome, app.ProjectRoot)
	for _, e := range perrs {
		fmt.Fprintln(errOut, "warn: profile:", e)
	}
	d.Profiles = profiles

	stats, statsErr := app.Store.ModeStats(recentLimit)
	if statsErr != nil {
		fmt.Fprintln(errOut, "warn: mode stats:", statsErr)
		stats = map[string]*store.ModeStat{}
	}
	d.Stats = stats

	alerts, alertErr := app.Store.ListSentinelAlertsSince(time.Now().Add(-24*time.Hour), alertLimit)
	if alertErr != nil {
		fmt.Fprintln(errOut, "warn: sentinel alerts:", alertErr)
	}
	d.Alerts = alerts

	board := improve.NewBoard(app.Store)
	ideas, ideaErr := board.List(improve.ListOptions{Limit: 500, Newest: true})
	if ideaErr != nil {
		fmt.Fprintln(errOut, "warn: ideas:", ideaErr)
	}
	d.Ideas = ideas

	retros, rerr := improve.ListRetrospectives(app.ProjectRoot)
	if rerr != nil {
		fmt.Fprintln(errOut, "warn: retrospectives:", rerr)
	}
	d.Retros = retros

	buckets, cerr := app.Store.CostRollups(time.Now().Add(-24*time.Hour), "")
	if cerr != nil {
		fmt.Fprintln(errOut, "warn: cost rollups:", cerr)
	}
	d.CostBuckets24h = buckets
	return d
}

// renderDashThreads prints the Active threads section. A missing or
// unreadable state.json is a warning, not an error — `uta dash` should
// still complete the other sections.
func renderDashThreads(out, errOut io.Writer, app *App) {
	fmt.Fprintln(out, "== Active threads ==")
	idx, err := state.Load(app.StateDir)
	if err != nil {
		fmt.Fprintln(errOut, "warn: load threads:", err)
		fmt.Fprintln(out, "(no thread state available)")
		return
	}
	if idx == nil || len(idx.Threads) == 0 {
		fmt.Fprintln(out, "(no threads yet — create one with 'uta thread new <name>')")
		return
	}
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ACTIVE\tNAME\tMODE\tLAST USED\tLAST SESSION\tID")
	for _, t := range idx.Threads {
		marker := " "
		if t.ID == idx.ActiveThread {
			marker = "*"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n",
			marker, t.Name, dashIfEmpty(t.Mode),
			t.LastUsedAt.UTC().Format(time.RFC3339),
			dashIfEmpty(shortID(t.ActiveSessionID)),
			shortID(t.ID),
		)
	}
	_ = tw.Flush()
}

// renderDashSessions prints the per-mode recent-session block. Modes
// known to the profile loader but never used are still listed (with a
// "no sessions yet" marker) so the operator can see the discovery
// surface even on a fresh repo. The unattributed bucket ("") is
// appended last so it doesn't visually drown the profile-attached rows.
func renderDashSessions(out io.Writer, profiles []*profile.MissionProfile, stats map[string]*store.ModeStat, recentLimit int) {
	fmt.Fprintln(out, "== Recent sessions per mode ==")

	// Stable order: profile-loader order first, then any mode_name that
	// appears in stats but has no profile (e.g. a renamed profile), then
	// the "" bucket.
	known := make(map[string]bool, len(profiles))
	order := make([]string, 0, len(profiles))
	for _, p := range profiles {
		known[p.Name] = true
		order = append(order, p.Name)
	}
	extra := make([]string, 0)
	for name := range stats {
		if name == "" || known[name] {
			continue
		}
		extra = append(extra, name)
	}
	sort.Strings(extra)
	order = append(order, extra...)
	if _, ok := stats[""]; ok {
		order = append(order, "")
	}

	for _, name := range order {
		ms := stats[name]
		header := name
		if header == "" {
			header = "(no mode)"
		}
		total := 0
		var subtaskSecs float64
		if ms != nil {
			total = ms.TotalSessions
			subtaskSecs = ms.SubtaskSeconds
		}
		fmt.Fprintf(out, "\n[%s]  total=%d  subtask_seconds=%.1f\n",
			header, total, subtaskSecs)
		if ms == nil || len(ms.RecentSessions) == 0 {
			fmt.Fprintln(out, "  (no sessions yet)")
			continue
		}
		tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "  ID\tCREATED\tWORKER\tSTATUS\tGOAL")
		shown := ms.RecentSessions
		if recentLimit > 0 && len(shown) > recentLimit {
			shown = shown[:recentLimit]
		}
		for _, s := range shown {
			fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\t%s\n",
				shortID(s.ID),
				s.CreatedAt.UTC().Format(time.RFC3339),
				s.Worker, s.Status,
				truncateLine(s.Goal, 60),
			)
		}
		_ = tw.Flush()
	}
}

// renderDashCost prints the 24h cost rollup grouped by mode + provider.
// The intent is at-a-glance "where did the dollar go" — the longer view
// (per-day, longer windows, JSON output) lives behind `uta perf --cost`.
// Empty list renders a tidy "no usage" line so the section is
// self-explanatory on a quiet project.
func renderDashCost(out io.Writer, buckets []store.CostBucket) {
	fmt.Fprintln(out, "== Cost (last 24h) ==")
	if len(buckets) == 0 {
		fmt.Fprintln(out, "(no completed subtasks with recorded usage)")
		return
	}
	// Roll up per (mode, provider) — flatten the day dimension since the
	// dashboard already filters to 24h.
	type key struct{ Mode, Provider string }
	flat := map[key]*store.CostBucket{}
	var totalCalls int
	var totalIn, totalOut, totalCents int64
	for _, b := range buckets {
		k := key{Mode: b.ModeName, Provider: b.Provider}
		entry, ok := flat[k]
		if !ok {
			entry = &store.CostBucket{ModeName: b.ModeName, Provider: b.Provider}
			flat[k] = entry
		}
		entry.Calls += b.Calls
		entry.TokensIn += b.TokensIn
		entry.TokensOut += b.TokensOut
		entry.USDCents += b.USDCents
		totalCalls += b.Calls
		totalIn += b.TokensIn
		totalOut += b.TokensOut
		totalCents += b.USDCents
	}
	rows := make([]*store.CostBucket, 0, len(flat))
	for _, r := range flat {
		rows = append(rows, r)
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].USDCents != rows[j].USDCents {
			return rows[i].USDCents > rows[j].USDCents
		}
		return rows[i].Provider < rows[j].Provider
	})
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "MODE\tPROVIDER\tCALLS\tTOKENS_IN\tTOKENS_OUT\tUSD")
	for _, r := range rows {
		mode := r.ModeName
		if mode == "" {
			mode = "(none)"
		}
		fmt.Fprintf(tw, "%s\t%s\t%d\t%d\t%d\t%s\n",
			mode, r.Provider, r.Calls, r.TokensIn, r.TokensOut, formatUSDCents(r.USDCents))
	}
	fmt.Fprintf(tw, "TOTAL\t\t%d\t%d\t%d\t%s\n",
		totalCalls, totalIn, totalOut, formatUSDCents(totalCents))
	_ = tw.Flush()
}

// renderDashAlerts prints persisted sentinel_alert rows from the last
// 24h, newest-first. Empty list renders a tidy "no alerts" line so the
// section is self-explanatory on a quiet project.
func renderDashAlerts(out io.Writer, alerts []store.SentinelAlertRow) {
	fmt.Fprintln(out, "== Sentinel alerts (last 24h) ==")
	if len(alerts) == 0 {
		fmt.Fprintln(out, "(no alerts in the last 24h)")
		return
	}
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "WHEN\tSEVERITY\tRULE\tMODE\tSESSION\tMESSAGE")
	for _, a := range alerts {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n",
			a.Ts.UTC().Format(time.RFC3339),
			dashIfEmpty(a.Severity),
			dashIfEmpty(a.Rule),
			dashIfEmpty(a.ModeName),
			shortID(a.SessionID),
			truncateLine(a.Message, 60),
		)
	}
	_ = tw.Flush()
}

// renderDashIdeas groups ideas by a tag prefix (default "mode") and
// prints status counts per group. An idea with no matching tag falls
// into "(untagged)" so totals reconcile. tagKey is matched
// case-insensitively against the literal prefix "<tagKey>:".
func renderDashIdeas(out io.Writer, ideas []*improve.Idea, tagKey string) {
	fmt.Fprintln(out, "== Idea board per mode ==")
	if tagKey == "" {
		tagKey = "mode"
	}
	prefix := strings.ToLower(tagKey) + ":"

	// group name → status → count
	groups := map[string]map[string]int{}
	groupOrder := []string{}
	addToGroup := func(group, status string) {
		m, ok := groups[group]
		if !ok {
			m = map[string]int{}
			groups[group] = m
			groupOrder = append(groupOrder, group)
		}
		m[status]++
	}

	if len(ideas) == 0 {
		fmt.Fprintln(out, "(no ideas on the board)")
		return
	}
	for _, idea := range ideas {
		matched := false
		for _, tag := range idea.Tags {
			lo := strings.ToLower(tag)
			if strings.HasPrefix(lo, prefix) {
				group := strings.TrimSpace(tag[len(prefix):])
				if group == "" {
					group = "(empty)"
				}
				addToGroup(group, idea.Status)
				matched = true
				break
			}
		}
		if !matched {
			addToGroup("(untagged)", idea.Status)
		}
	}

	sort.SliceStable(groupOrder, func(i, j int) bool {
		// Push "(untagged)" to the end so tagged groups read first.
		a, b := groupOrder[i], groupOrder[j]
		if a == "(untagged)" {
			return false
		}
		if b == "(untagged)" {
			return true
		}
		return a < b
	})

	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "GROUP\tPROPOSED\tACCEPTED\tIN_PROGRESS\tDONE\tFAILED\tREJECTED\tTOTAL")
	for _, g := range groupOrder {
		m := groups[g]
		total := 0
		for _, n := range m {
			total += n
		}
		fmt.Fprintf(tw, "%s\t%d\t%d\t%d\t%d\t%d\t%d\t%d\n",
			g,
			m[improve.StatusProposed],
			m[improve.StatusAccepted],
			m[improve.StatusInProgress],
			m[improve.StatusDone],
			m[improve.StatusFailed],
			m[improve.StatusRejected],
			total,
		)
	}
	_ = tw.Flush()
}

// renderDashRetrospectives prints the per-mode retrospective files discovered
// under <project>/.uta/context/retrospectives/, newest-first. Outside a
// project (no project root), the section is omitted entirely — there is no
// global retrospective dir.
func renderDashRetrospectives(out io.Writer, retros []improve.RetrospectiveFile, inProject bool) {
	if !inProject {
		return
	}
	fmt.Fprintln(out, "== Retrospectives ==")
	if len(retros) == 0 {
		fmt.Fprintln(out, "(none yet — set retrospective_every: N on a profile to enable)")
		return
	}
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "MODIFIED\tMODE\tFILE")
	for _, r := range retros {
		fmt.Fprintf(tw, "%s\t%s\t%s\n",
			r.ModTime.UTC().Format(time.RFC3339),
			dashIfEmpty(r.Mode),
			r.Name,
		)
	}
	_ = tw.Flush()
}

// renderDashPane dispatches to the registered per-mode renderer. Modes
// not in paneRegistry fall back to the generic ideas-and-sessions
// renderer — that's how user-defined profiles get a pane without
// having to register a specialized one. An empty name is a no-op.
func renderDashPane(out io.Writer, d *dashData, name, tagKey string) {
	if strings.TrimSpace(name) == "" {
		return
	}
	if spec, ok := paneRegistry[name]; ok {
		spec.render(out, d, tagKey)
		return
	}
	renderDashGeneric(out, d, tagKey, name)
}

// genericPane is the JSON-serializable shape of the per-mode pane. It
// works for any mode name — the prototypical "dev" pane is just a
// generic pane bound to mode="dev". Specialized panes (ops, audit,
// research) extend the same idea-and-session view with extra
// workflow-specific fields.
type genericPane struct {
	Mode             string         `json:"mode"`
	IdeasByStatus    map[string]int `json:"ideas_by_status"`
	IdeasTotal       int            `json:"ideas_total"`
	IdeasDoneRatio   float64        `json:"ideas_done_ratio"`
	SessionsTotal    int            `json:"sessions_total"`
	SessionsByStatus map[string]int `json:"sessions_by_status"`
	TestPassRate     float64        `json:"test_pass_rate"`
}

// devPane is kept as a type alias for backward-compatible callers.
// Removed once all references migrate to genericPane in the next cycle.
type devPane = genericPane

// buildGenericPane builds the standard ideas-and-sessions view for any
// mode name. Generic-first: no hardcoded "dev" / "ops" / "audit". Pass
// any string; the helper filters ideas by `<tagKey>:<modeName>` tag
// match and reads sessions from d.Stats[modeName].
func buildGenericPane(d *dashData, tagKey, modeName string) genericPane {
	p := genericPane{
		Mode:             modeName,
		IdeasByStatus:    map[string]int{},
		SessionsByStatus: map[string]int{},
	}
	prefix := normalizeTagPrefix(tagKey)
	for _, idea := range d.Ideas {
		if !ideaMatchesMode(idea, prefix, modeName) {
			continue
		}
		p.IdeasByStatus[idea.Status]++
		p.IdeasTotal++
	}
	if p.IdeasTotal > 0 {
		p.IdeasDoneRatio = float64(p.IdeasByStatus[improve.StatusDone]) / float64(p.IdeasTotal)
	}
	if ms, ok := d.Stats[modeName]; ok && ms != nil {
		p.SessionsTotal = ms.TotalSessions
		for _, s := range ms.RecentSessions {
			p.SessionsByStatus[s.Status]++
		}
		if pass, total := passAndTotal(ms.RecentSessions); total > 0 {
			p.TestPassRate = float64(pass) / float64(total)
		}
	}
	return p
}

// buildDevPane is a backward-compatible shim. Prefer buildGenericPane.
func buildDevPane(d *dashData, tagKey string) genericPane {
	return buildGenericPane(d, tagKey, "dev")
}

// renderDashDev shows the burndown of dev ideas plus a derived test pass-rate
// across recent dev sessions. With no dev data the pane still renders header
// rows and (0)/(n/a) markers so an empty project doesn't print as a blank
// section — sparse-data renderability is a stated acceptance criterion.
//
// modeName is the profile / mode being rendered. Generic: works for any
// name, not just "dev". For ergonomics the headline uses Title-case so
// "ops" renders as "Ops mode pane".
func renderDashGeneric(out io.Writer, d *dashData, tagKey, modeName string) {
	p := buildGenericPane(d, tagKey, modeName)
	titleMode := modeName
	if titleMode == "" {
		titleMode = "(unnamed)"
	} else {
		titleMode = strings.ToUpper(titleMode[:1]) + titleMode[1:]
	}
	fmt.Fprintf(out, "== %s mode pane ==\n", titleMode)

	fmt.Fprintf(out, "  Idea burndown (%s%s):\n", normalizeTagPrefix(tagKey), modeName)
	if p.IdeasTotal == 0 {
		fmt.Fprintf(out, "    (no %s ideas yet — tag with '%s%s' to populate)\n",
			modeName, normalizeTagPrefix(tagKey), modeName)
	} else {
		tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "    PROPOSED\tACCEPTED\tIN_PROGRESS\tDONE\tFAILED\tREJECTED\tTOTAL\tDONE_RATIO")
		fmt.Fprintf(tw, "    %d\t%d\t%d\t%d\t%d\t%d\t%d\t%.0f%%\n",
			p.IdeasByStatus[improve.StatusProposed],
			p.IdeasByStatus[improve.StatusAccepted],
			p.IdeasByStatus[improve.StatusInProgress],
			p.IdeasByStatus[improve.StatusDone],
			p.IdeasByStatus[improve.StatusFailed],
			p.IdeasByStatus[improve.StatusRejected],
			p.IdeasTotal,
			p.IdeasDoneRatio*100,
		)
		_ = tw.Flush()
	}

	fmt.Fprintf(out, "  Sessions (mode=%s):\n", modeName)
	if p.SessionsTotal == 0 {
		fmt.Fprintf(out, "    (no %s sessions yet — run with 'uta run --mode %s')\n", modeName, modeName)
		return
	}
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "    TOTAL\tCOMPLETED\tPARTIAL\tFAILED\tCANCELLED\tRUNNING\tTEST_PASS_RATE")
	rate := "n/a"
	if anyRecent(d.Stats[modeName]) {
		rate = fmt.Sprintf("%.0f%%", p.TestPassRate*100)
	}
	fmt.Fprintf(tw, "    %d\t%d\t%d\t%d\t%d\t%d\t%s\n",
		p.SessionsTotal,
		p.SessionsByStatus["completed"],
		p.SessionsByStatus["partial"],
		p.SessionsByStatus["failed"],
		p.SessionsByStatus["cancelled"],
		p.SessionsByStatus["running"],
		rate,
	)
	_ = tw.Flush()
}

// renderDashDev is a backward-compatible shim that renders the "dev"
// pane via the generic renderer. Prefer renderDashGeneric.
func renderDashDev(out io.Writer, d *dashData, tagKey string) {
	renderDashGeneric(out, d, tagKey, "dev")
}

// opsPane is the JSON-serializable shape of the ops pane.
type opsPane struct {
	Mode             string     `json:"mode"`
	OrgStateExists   bool       `json:"org_state_exists"`
	OrgStateLastSet  *time.Time `json:"org_state_last_updated,omitempty"`
	SourceEmails     int        `json:"source_emails"`
	PendingDecisions int        `json:"pending_decisions"`
	OrgStateBytes    int64      `json:"org_state_bytes"`
	SessionsTotal    int        `json:"sessions_total"`
	DraftsCreated    int        `json:"drafts_created"`
	OrgStatePath     string     `json:"org_state_path,omitempty"`
}

// buildOpsPaneFor builds the ops-shaped pane (ORG_STATE.md + drafts +
// session totals) for the given mode name. Generic-first: the helper
// works for any profile that runs publishing workflows, not just one
// literally named "ops". The mode name affects:
//   - p.Mode (echoed back in the JSON)
//   - which session-stat bucket is read (d.Stats[modeName])
//   - which session-rows are queried for draft counts (mode_name = ?)
func buildOpsPaneFor(d *dashData, modeName string) opsPane {
	p := opsPane{Mode: modeName}
	if d.App.ContextDir != "" {
		p.OrgStatePath = orgstate.Path(d.App.ContextDir)
		if info, err := os.Stat(p.OrgStatePath); err == nil {
			p.OrgStateExists = true
			p.OrgStateBytes = info.Size()
		}
		if p.OrgStateExists {
			if st, err := orgstate.Load(d.App.ContextDir); err == nil && st != nil {
				if !st.Frontmatter.LastUpdated.IsZero() {
					t := st.Frontmatter.LastUpdated
					p.OrgStateLastSet = &t
				}
				p.SourceEmails = len(st.Frontmatter.SourceEmails)
				p.PendingDecisions = len(st.Frontmatter.PendingDecisions)
			}
		}
	}
	if ms, ok := d.Stats[modeName]; ok && ms != nil {
		p.SessionsTotal = ms.TotalSessions
	}
	p.DraftsCreated = countDraftsForMode(d.App, modeName)
	return p
}

// buildOpsPane is the backward-compatible shim for the literal "ops" mode.
// Prefer buildOpsPaneFor when the caller can name the mode.
func buildOpsPane(d *dashData) opsPane {
	return buildOpsPaneFor(d, "ops")
}

// renderDashOps reports the ops-mode artifact health: ORG_STATE.md
// presence + freshness, source-email and pending-decision counts, draft
// creations seen on the trajectory, and ops session totals.
func renderDashOps(out io.Writer, d *dashData) {
	p := buildOpsPane(d)
	fmt.Fprintln(out, "== Ops mode pane ==")
	if !d.App.InProject() {
		fmt.Fprintln(out, "  (no project — ORG_STATE.md lives under <project>/.uta/context/)")
	} else if !p.OrgStateExists {
		fmt.Fprintln(out, "  ORG_STATE.md: (not created yet — ops mode populates on first run)")
	} else {
		tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "  ORG_STATE.md\tLAST_UPDATED\tEMAILS\tPENDING\tBYTES")
		last := "-"
		if p.OrgStateLastSet != nil {
			last = p.OrgStateLastSet.UTC().Format(time.RFC3339)
		}
		fmt.Fprintf(tw, "  present\t%s\t%d\t%d\t%d\n",
			last, p.SourceEmails, p.PendingDecisions, p.OrgStateBytes)
		_ = tw.Flush()
	}

	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "  SESSIONS_TOTAL\tDRAFTS_CREATED")
	fmt.Fprintf(tw, "  %d\t%d\n", p.SessionsTotal, p.DraftsCreated)
	_ = tw.Flush()
}

// auditPane is the JSON-serializable shape of the audit pane.
type auditPane struct {
	Mode              string                    `json:"mode"`
	FindingsExist     bool                      `json:"findings_exist"`
	Iteration         int                       `json:"iteration,omitempty"`
	ProducedAt        *time.Time                `json:"produced_at,omitempty"`
	TotalFindings     int                       `json:"total_findings"`
	BySeverity        map[string]int            `json:"by_severity"`
	ByFile            map[string]int            `json:"by_file"`
	Heatmap           map[string]map[string]int `json:"heatmap_severity_by_file"`
	SessionsTotal     int                       `json:"sessions_total"`
	HighSeverityFiles []string                  `json:"high_severity_files,omitempty"`
	FindingsPath      string                    `json:"findings_path,omitempty"`
}

func buildAuditPane(d *dashData) auditPane {
	p := auditPane{
		Mode:       "audit",
		BySeverity: map[string]int{},
		ByFile:     map[string]int{},
		Heatmap:    map[string]map[string]int{},
	}
	if d.App.ContextDir != "" {
		p.FindingsPath = filepath.Join(d.App.ContextDir, "findings.json")
		if rep, ok := loadFindingsReport(p.FindingsPath); ok {
			p.FindingsExist = true
			p.Iteration = rep.Iteration
			t := rep.ProducedAt
			p.ProducedAt = &t
			for _, f := range rep.Findings {
				sev := string(f.Severity)
				if sev == "" {
					sev = string(engine.SevUnknown)
				}
				file := f.File
				if file == "" {
					file = "(no-file)"
				}
				p.BySeverity[sev]++
				p.ByFile[file]++
				if _, ok := p.Heatmap[file]; !ok {
					p.Heatmap[file] = map[string]int{}
				}
				p.Heatmap[file][sev]++
				p.TotalFindings++
				if engine.Severity(sev) == engine.SevHigh {
					p.HighSeverityFiles = append(p.HighSeverityFiles, file)
				}
			}
		}
	}
	if ms, ok := d.Stats["audit"]; ok && ms != nil {
		p.SessionsTotal = ms.TotalSessions
	}
	return p
}

// renderDashAudit prints a severity-by-file heatmap from the latest
// findings.json in the context directory. Falls back to a guidance line
// when no audit has produced findings yet.
func renderDashAudit(out io.Writer, d *dashData) {
	p := buildAuditPane(d)
	fmt.Fprintln(out, "== Audit mode pane ==")
	if !d.App.InProject() {
		fmt.Fprintln(out, "  (no project — findings.json lives under <project>/.uta/context/)")
		return
	}
	if !p.FindingsExist {
		fmt.Fprintln(out, "  (no findings.json yet — run 'uta audit <path>' to generate one)")
		fmt.Fprintf(out, "  Audit sessions to date: %d\n", p.SessionsTotal)
		return
	}

	producedAt := "-"
	if p.ProducedAt != nil {
		producedAt = p.ProducedAt.UTC().Format(time.RFC3339)
	}
	fmt.Fprintf(out, "  iteration=%d  produced_at=%s  total_findings=%d  audit_sessions=%d\n",
		p.Iteration, producedAt, p.TotalFindings, p.SessionsTotal)

	if p.TotalFindings == 0 {
		fmt.Fprintln(out, "  (no findings in the latest iteration)")
		return
	}

	// Severity totals.
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "  HIGH\tMEDIUM\tLOW\tINFO\tUNKNOWN")
	fmt.Fprintf(tw, "  %d\t%d\t%d\t%d\t%d\n",
		p.BySeverity[string(engine.SevHigh)],
		p.BySeverity[string(engine.SevMedium)],
		p.BySeverity[string(engine.SevLow)],
		p.BySeverity[string(engine.SevInfo)],
		p.BySeverity[string(engine.SevUnknown)],
	)
	_ = tw.Flush()

	// Heatmap by file (sorted by total descending, then file name).
	files := make([]string, 0, len(p.Heatmap))
	for f := range p.Heatmap {
		files = append(files, f)
	}
	sort.Slice(files, func(i, j int) bool {
		if p.ByFile[files[i]] != p.ByFile[files[j]] {
			return p.ByFile[files[i]] > p.ByFile[files[j]]
		}
		return files[i] < files[j]
	})

	fmt.Fprintln(out, "  Heatmap (file × severity):")
	hw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(hw, "    FILE\tH\tM\tL\tI\tU\tTOTAL")
	for _, f := range files {
		row := p.Heatmap[f]
		fmt.Fprintf(hw, "    %s\t%d\t%d\t%d\t%d\t%d\t%d\n",
			truncateLine(f, 50),
			row[string(engine.SevHigh)],
			row[string(engine.SevMedium)],
			row[string(engine.SevLow)],
			row[string(engine.SevInfo)],
			row[string(engine.SevUnknown)],
			p.ByFile[f],
		)
	}
	_ = hw.Flush()
}

// researchPane is the JSON-serializable shape of the research pane.
type researchPane struct {
	Mode            string     `json:"mode"`
	DBExists        bool       `json:"db_exists"`
	LastUpdated     *time.Time `json:"last_updated,omitempty"`
	Queries         int        `json:"queries"`
	Sources         int        `json:"sources"`
	Claims          int        `json:"claims"`
	BytesOnDisk     int64      `json:"bytes_on_disk"`
	SessionsTotal   int        `json:"sessions_total"`
	UncitedClaims   int        `json:"uncited_claims"`
	ClaimsPerSource float64    `json:"claims_per_source"`
	ResearchDBPath  string     `json:"research_db_path,omitempty"`
}

func buildResearchPane(d *dashData) researchPane {
	p := researchPane{Mode: "research"}
	if d.App.ContextDir != "" {
		p.ResearchDBPath = researchdb.Path(d.App.ContextDir)
		if info, err := os.Stat(p.ResearchDBPath); err == nil {
			p.DBExists = true
			p.BytesOnDisk = info.Size()
		}
		if p.DBExists {
			if st, err := researchdb.Load(d.App.ContextDir); err == nil && st != nil {
				if !st.Frontmatter.LastUpdated.IsZero() {
					t := st.Frontmatter.LastUpdated
					p.LastUpdated = &t
				}
				p.Queries = len(st.Frontmatter.Queries)
				p.Sources = len(st.Frontmatter.Sources)
				p.Claims = len(st.Frontmatter.Claims)
				for _, c := range st.Frontmatter.Claims {
					if len(c.Sources) == 0 {
						p.UncitedClaims++
					}
				}
				if p.Sources > 0 {
					p.ClaimsPerSource = float64(p.Claims) / float64(p.Sources)
				}
			}
		}
	}
	if ms, ok := d.Stats["research"]; ok && ms != nil {
		p.SessionsTotal = ms.TotalSessions
	}
	return p
}

// renderDashResearch prints the research-mode source-coverage block:
// queries / sources / claims counts, on-disk size of RESEARCH_DATABASE.md,
// and a claims-per-source ratio so an operator can spot a thin-coverage
// ledger at a glance.
func renderDashResearch(out io.Writer, d *dashData) {
	p := buildResearchPane(d)
	fmt.Fprintln(out, "== Research mode pane ==")
	if !d.App.InProject() {
		fmt.Fprintln(out, "  (no project — RESEARCH_DATABASE.md lives under <project>/.uta/context/)")
		return
	}
	if !p.DBExists {
		fmt.Fprintln(out, "  (no RESEARCH_DATABASE.md yet — run 'uta run --mode research' to seed)")
		fmt.Fprintf(out, "  Research sessions to date: %d\n", p.SessionsTotal)
		return
	}
	last := "-"
	if p.LastUpdated != nil {
		last = p.LastUpdated.UTC().Format(time.RFC3339)
	}
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "  LAST_UPDATED\tQUERIES\tSOURCES\tCLAIMS\tUNCITED\tBYTES\tSESSIONS\tCLAIMS/SOURCE")
	fmt.Fprintf(tw, "  %s\t%d\t%d\t%d\t%d\t%d\t%d\t%.2f\n",
		last, p.Queries, p.Sources, p.Claims, p.UncitedClaims,
		p.BytesOnDisk, p.SessionsTotal, p.ClaimsPerSource)
	_ = tw.Flush()
}

// emitDashJSON encodes a JSON snapshot of the dashboard. With no
// modeFilter, the envelope contains every registered pane keyed by mode
// name; with a modeFilter, just that single pane object is emitted.
// Keys come from paneRegistry rather than a hardcoded struct, so adding
// a new mode shows up in --json automatically. Tooling that pins on
// specific keys should treat the schema as additive.
func emitDashJSON(out io.Writer, d *dashData, modeFilter, tagKey string) error {
	enc := json.NewEncoder(out)
	enc.SetIndent("", "  ")
	if modeFilter != "" {
		spec, ok := paneRegistry[modeFilter]
		if !ok {
			return fmt.Errorf("unknown mode %q: expected one of %s", modeFilter, strings.Join(registeredPaneNames(), ", "))
		}
		return enc.Encode(spec.build(d, tagKey))
	}
	envelope := make(map[string]any, len(paneRegistry))
	for name, spec := range paneRegistry {
		envelope[name] = spec.build(d, tagKey)
	}
	return enc.Encode(envelope)
}

// loadFindingsReport reads <ctxDir>/findings.json as a FindingsReport.
// Returns (rep, true) only when the file exists and parses cleanly; any
// other outcome (missing, malformed, IO error) yields (zero, false) so
// the dashboard can fall back to its "no findings yet" hint.
func loadFindingsReport(path string) (engine.FindingsReport, bool) {
	var rep engine.FindingsReport
	data, err := os.ReadFile(path)
	if err != nil {
		return rep, false
	}
	if err := json.Unmarshal(data, &rep); err != nil {
		return rep, false
	}
	return rep, true
}

// countDraftsForMode counts distinct draft-creation tool invocations seen
// on the trajectory for sessions in the named mode. Drafts published is
// a soft metric — the engine doesn't track it as a first-class counter,
// so we approximate via tool-event names extracted from each event's
// JSON payload.
//
// Each logical call is counted once: subtask_tool_call (the canonical
// worker-tool path) OR tool_completed (the Reflector path's terminal
// success event) — but never both, since the two strategies are mutually
// exclusive for any given session. Restricting to those kinds avoids
// double-counting the started/completed pair Reflector emits.
//
// Generic-first: the mode name is a parameter, not hardcoded. A user-
// defined profile named "publishing" or "comms" gets the same draft
// count when its sessions ran the equivalent tool — no need to rename
// the profile to "ops" to participate.
//
// Returns 0 on any error (including the table being missing on a brand-new
// project) or when modeName is empty. The Gmail tool name is
// `mcp__claude_ai_Gmail__create_draft` or similar namespaced variants,
// so we match on the suffix.
func countDraftsForMode(app *App, modeName string) int {
	if app == nil || app.Store == nil || app.Store.DB == nil {
		return 0
	}
	if strings.TrimSpace(modeName) == "" {
		return 0
	}
	const draftNameLike = "%create_draft"
	row := app.Store.DB.QueryRow(
		`SELECT COUNT(*)
		   FROM trajectory_events e
		   JOIN sessions s ON s.id = e.session_id
		  WHERE s.mode_name = ?
		    AND e.kind IN ('subtask_tool_call', 'tool_completed')
		    AND json_extract(e.payload_json, '$.name') LIKE ?`, modeName, draftNameLike,
	)
	var n int
	if err := row.Scan(&n); err != nil {
		return 0
	}
	return n
}

// countOpsDrafts is a backward-compatible shim that counts drafts for the
// literal "ops" mode. Prefer countDraftsForMode.
func countOpsDrafts(app *App) int {
	return countDraftsForMode(app, "ops")
}

// passAndTotal counts completed sessions out of total terminal sessions in
// the slice. "Terminal" here is anything that's not running. A run still
// in flight is not yet a pass or a fail, so it's excluded from the
// denominator — otherwise the pass-rate would dip during a long run.
func passAndTotal(sessions []store.Session) (pass, total int) {
	for _, s := range sessions {
		switch s.Status {
		case "completed":
			pass++
			total++
		case "partial", "failed", "cancelled":
			total++
		}
	}
	return pass, total
}

func anyRecent(ms *store.ModeStat) bool {
	return ms != nil && len(ms.RecentSessions) > 0
}

func normalizeTagPrefix(tagKey string) string {
	if tagKey == "" {
		tagKey = "mode"
	}
	return strings.ToLower(tagKey) + ":"
}

// ideaMatchesMode reports whether idea carries a "<prefix><mode>" tag,
// case-insensitively. With no tags, the idea is intentionally not matched
// — the per-mode pane only shows ideas the operator has explicitly tagged
// for that mode, mirroring the grouping done in renderDashIdeas.
func ideaMatchesMode(idea *improve.Idea, prefix, mode string) bool {
	for _, tag := range idea.Tags {
		lo := strings.ToLower(tag)
		if !strings.HasPrefix(lo, prefix) {
			continue
		}
		if strings.TrimSpace(lo[len(prefix):]) == mode {
			return true
		}
	}
	return false
}
