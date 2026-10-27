package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/unleashtheagents/uta/internal/engine"
	"github.com/unleashtheagents/uta/internal/policy"
	"github.com/unleashtheagents/uta/internal/provider/builtin"
)

// fileRefRe finds @path tokens: "@main.go", "@src/", "@docs/plan.md,".
var fileRefRe = regexp.MustCompile(`(^|\s)@([^\s@]+)`)

const (
	maxRefFileBytes = 48 * 1024
	maxRefDirEntry  = 200
)

// expandFileRefs replaces @path tokens whose path exists under workdir
// with the bare path and appends the file (or directory listing) to the
// goal. Tokens that do not resolve are left untouched, so "@gemini" in the
// middle of a sentence stays a word. It returns the expanded goal and a
// label per attachment.
func expandFileRefs(goal, workdir string) (string, []string) {
	if !strings.Contains(goal, "@") {
		return goal, nil
	}
	if workdir == "" {
		workdir, _ = os.Getwd()
	}
	var blocks, labels []string
	seen := map[string]bool{}
	out := fileRefRe.ReplaceAllStringFunc(goal, func(tok string) string {
		m := fileRefRe.FindStringSubmatch(tok)
		lead, raw := m[1], m[2]
		p := strings.TrimRight(raw, ",.;:!?)")
		trail := raw[len(p):]
		if p == "" || filepath.IsAbs(p) || strings.HasPrefix(p, "..") {
			return tok
		}
		full := filepath.Join(workdir, p)
		st, err := os.Stat(full)
		if err != nil {
			return tok
		}
		if !seen[p] {
			seen[p] = true
			if st.IsDir() {
				entries, _ := os.ReadDir(full)
				var names []string
				for i, e := range entries {
					if i >= maxRefDirEntry {
						names = append(names, fmt.Sprintf("... [%d more]", len(entries)-maxRefDirEntry))
						break
					}
					n := e.Name()
					if e.IsDir() {
						n += "/"
					}
					names = append(names, n)
				}
				blocks = append(blocks, fmt.Sprintf("--- directory %s (%d entries) ---\n%s", p, len(entries), strings.Join(names, "\n")))
				labels = append(labels, fmt.Sprintf("%s/ (%d entries)", strings.TrimSuffix(p, "/"), len(entries)))
			} else {
				data, rerr := os.ReadFile(full)
				if rerr != nil {
					return tok
				}
				note := ""
				if len(data) > maxRefFileBytes {
					data = data[:maxRefFileBytes]
					note = "\n... [truncated]"
				}
				blocks = append(blocks, fmt.Sprintf("--- file %s ---\n%s%s", p, string(data), note))
				labels = append(labels, fmt.Sprintf("%s (%s)", p, humanBytes(st.Size())))
			}
		}
		return lead + p + trail
	})
	if len(blocks) == 0 {
		return goal, nil
	}
	return out + "\n\n" + strings.Join(blocks, "\n\n"), labels
}

func (s *utaShell) effectiveWorkdir() string {
	if s.workdir != "" {
		return s.workdir
	}
	cwd, _ := os.Getwd()
	return cwd
}

// HasAgent implements shellBackend.
func (s *utaShell) HasAgent(name string) bool {
	_, ok := s.app.Registry.Get(name)
	return ok
}

// Vision implements shellBackend: one Super-VL call, answer printed and
// kept for the next goal.
func (s *utaShell) Vision(ctx context.Context, w io.Writer, imagePath, prompt string) error {
	if strings.TrimSpace(os.Getenv("NEBIUS_API_KEY")) == "" {
		return fmt.Errorf("NEBIUS_API_KEY is not set (uta setup)")
	}
	full := imagePath
	if !filepath.IsAbs(full) {
		full = filepath.Join(s.effectiveWorkdir(), imagePath)
	}
	res, err := describeImage(ctx, s.app, full, prompt)
	if err != nil {
		return err
	}
	printWrapped(w, res.Text)
	fmt.Fprintf(s.errw, "[uta] %s read %s in %d ms (%d in / %d out tokens, $%.4f) — attached to your next goal\n",
		shortNebiusModel(res.Model), filepath.Base(imagePath), res.LatencyMS, res.TokensIn, res.TokensOut, float64(res.USDCents)/100)
	s.pendingContext = append(s.pendingContext, fmt.Sprintf("Image analysis of %s (by %s):\n%s", imagePath, res.Model, res.Text))
	return nil
}

// describeImage runs the vision call through the user-level spend guard so
// /image and `uta nebius see` count against the same daily cap as runs.
func describeImage(ctx context.Context, app *App, imagePath, prompt string) (builtin.VisionResult, error) {
	if app.Usage != nil {
		if err := app.Usage.BeforeCall(ctx, "nebius"); err != nil {
			return builtin.VisionResult{}, err
		}
	}
	res, err := (&builtin.Nebius{}).Describe(ctx, imagePath, prompt, nil)
	if app.Usage != nil && (res.TokensIn > 0 || res.TokensOut > 0) {
		app.Usage.AfterCall(ctx, engine.UsageRecord{Ts: time.Now(), Provider: "nebius", Role: "vision",
			TokensIn: res.TokensIn, TokensOut: res.TokensOut, USDCents: res.USDCents, Err: err})
	}
	return res, err
}

func (s *utaShell) lastRun() (string, error) {
	if s.lastSession != "" {
		return s.lastSession, nil
	}
	if s.sessionID != "" {
		return s.sessionID, nil
	}
	return "", fmt.Errorf("no run in this shell yet")
}

// Diff implements shellBackend.
func (s *utaShell) Diff(w io.Writer) error {
	sid, err := s.lastRun()
	if err != nil {
		return err
	}
	changes, err := sessionFileChanges(s.app, sid)
	if err != nil {
		return err
	}
	if len(changes) == 0 {
		fmt.Fprintf(w, "run %s changed no files (checkpoints are taken for write_file / edit_file through the permissions gate)\n", shortID(sid))
		return nil
	}
	printFileDiffs(w, changes, !s.noColor && os.Getenv("NO_COLOR") == "")
	return nil
}

// Rewind implements shellBackend.
func (s *utaShell) Rewind(w io.Writer) error {
	sid, err := s.lastRun()
	if err != nil {
		return err
	}
	changes, err := sessionFileChanges(s.app, sid)
	if err != nil {
		return err
	}
	if len(changes) == 0 {
		fmt.Fprintf(w, "run %s changed no files; nothing to rewind\n", shortID(sid))
		return nil
	}
	sort.SliceStable(changes, func(i, j int) bool { return changes[i].Path < changes[j].Path })
	n, err := undoChanges(w, changes, false)
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "rewound run %s: %d file(s) restored\n", shortID(sid), n)
	return nil
}

// Cost implements shellBackend.
func (s *utaShell) Cost(w io.Writer) error {
	sid, err := s.lastRun()
	if err != nil {
		return err
	}
	sess, err := s.app.Store.GetSession(sid)
	if err != nil {
		return err
	}
	events, err := s.app.Store.ListEvents(sid, 0, 0)
	if err != nil {
		return err
	}
	rep := buildNebiusCostReport(sess, events)
	if len(rep.ByModel) == 0 {
		fmt.Fprintf(w, "run %s made no Nebius calls\n", shortID(sid))
		return nil
	}
	return writeNebiusCostReport(w, rep)
}

// Permissions implements shellBackend.
func (s *utaShell) Permissions(w io.Writer) error {
	p, err := policy.LoadMerged(s.app.GlobalHome, s.effectiveWorkdir())
	if err != nil {
		return err
	}
	if len(p.Sources) == 0 {
		fmt.Fprintf(w, "no permissions files — defaults: read-only tools run, write_file / edit_file / run_command ask.\n(uta permissions init writes %s)\n", policy.ProjectFile(s.effectiveWorkdir()))
	} else {
		fmt.Fprintf(w, "sources: %s\n", strings.Join(p.Sources, ", "))
		for _, r := range p.Deny {
			fmt.Fprintf(w, "  deny   %s\n", r)
		}
		for _, r := range p.Ask {
			fmt.Fprintf(w, "  ask    %s\n", r)
		}
		for _, r := range p.Allow {
			fmt.Fprintf(w, "  allow  %s\n", r)
		}
	}
	if len(s.preApprove) > 0 {
		fmt.Fprintf(w, "pre-approved this shell (/tools): %s\n", strings.Join(s.preApprove, ", "))
	}
	return nil
}

// applyDirectMode turns a run request into a single worker turn: the goal
// is the one subtask, no planner, no synthesis. Later turns resume the
// same conversation through the provider, so this is the Claude Code /
// agy shape of interaction, on Nemotron Super with the full tool set and
// Nano compaction once the history grows.
func applyDirectMode(req *engine.RunRequest) {
	title := strings.TrimSpace(req.Goal)
	if len(title) > 72 {
		title = title[:72] + "..."
	}
	req.PreSetSubtasks = []engine.SubtaskSpec{{ID: "direct", Title: title, Prompt: req.Goal}}
	req.SkipSynthesis = true
	req.MaxParallel = 1
}
