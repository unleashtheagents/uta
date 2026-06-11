// Package shadow regression-tests prompt changes. When a MissionProfile
// (or persona, or vibe layered onto a profile) is edited, the old run that
// originally finished cleanly under the previous profile may now produce
// different output. Shadow replays the original goal against the new
// profile in a sandboxed workdir and reports the delta — token count,
// output diff, exit status — so the operator can decide whether the
// prompt change is acceptable or a regression.
//
// Shadow is intentionally read-only with respect to the production
// SQLite store: the replay supervisor writes to a throwaway DB and blob
// dir under os.TempDir, so a shadow run never pollutes session history.
// Auto-rollback on regression and parallel shadow runs are out of scope
// for v1 (operator decides; sequential is fine for now).
package shadow

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/unleashtheagents/uta/internal/engine"
	"github.com/unleashtheagents/uta/internal/profile"
	"github.com/unleashtheagents/uta/internal/provider"
	"github.com/unleashtheagents/uta/internal/store"
	"github.com/unleashtheagents/uta/internal/trajectory"
)

// DefaultDriftTokens is the per-session output-length delta (measured in
// rough token counts; see countTokens) at or above which a session is
// flagged as drifted in the report. Operators can override on three
// levels — Shadow.DriftTokens (per-Run, e.g. from a CLI flag) wins, then
// the per-profile ShadowDriftTokens setting, then this constant.
const DefaultDriftTokens = 50

// Shadow replays one historical session under a new MissionProfile and
// captures the delta between the original outcome and the replay.
//
// SourceSessionID must reference a row that exists in Deps.Store.
// ProfileBefore is informational only — it is recorded on the report so
// the reader can see which profile snapshot the replay is being compared
// against, but Shadow never re-runs the original session. ProfileAfter
// is the profile actually applied to the replay.
type Shadow struct {
	SourceSessionID string
	ProfileBefore   *profile.MissionProfile
	ProfileAfter    *profile.MissionProfile

	// WorkerName overrides the worker provider used for the replay. When
	// empty, the original session's worker is reused.
	WorkerName string

	// Workdir is the project directory to use as a sandbox for the
	// replay. Shadow does not create or destroy this directory; callers
	// that want isolation should supply a temp dir (e.g. via
	// SandboxWorkdir). Defaults to the original session's project root
	// (the cwd at replay time when not in a project).
	Workdir string

	// DriftTokens overrides DefaultDriftTokens for the "drifted" flag in
	// the resulting SessionDelta.
	DriftTokens int

	// Env is appended to the request env passed to the supervisor. Mirrors
	// engine.RunRequest.Env. The profile's own Env is merged on top via
	// engine.ApplyProfile.
	Env []string
}

// Deps is the wired infrastructure Shadow.Run needs.
type Deps struct {
	Store    *store.Store
	Blobs    *store.Blobs
	Registry *provider.Registry

	// Replay overrides the function used to execute the replay run. When
	// nil, a default implementation wired to the supplied Store/Blobs/
	// Registry runs the supervisor end-to-end. Tests inject a stub so the
	// shadow logic can be exercised without spawning provider CLIs.
	Replay ReplayFunc
}

// ReplayFunc executes one replay attempt and returns its outcome. The
// goal, worker, profile, workdir, and env are pre-resolved by Shadow.Run;
// implementations are responsible for spinning up an engine.Supervisor
// (or a stub thereof) and producing a SessionDelta with the per-replay
// fields filled in (NewFinalText, NewStatus, ReplayDuration, errors).
type ReplayFunc func(ctx context.Context, in ReplayInput) (ReplayOutput, error)

// ReplayInput is the pre-resolved replay configuration passed to a
// ReplayFunc.
type ReplayInput struct {
	Goal       string
	WorkerName string
	Profile    *profile.MissionProfile
	Workdir    string
	Env        []string
}

// ReplayOutput is what a ReplayFunc returns. FinalText is the synthesized
// final answer (or joined subtask output) of the replay. Status mirrors
// engine.RunResult.Status. Duration is the wall-clock time the replay
// took. Err, when non-nil, signals that the replay itself failed — Shadow
// records it in the SessionDelta but continues with the next session.
type ReplayOutput struct {
	FinalText string
	Status    string
	Duration  time.Duration
	Err       error
}

// SessionDelta is one row in a Report — the comparison between an
// original session and its replay.
type SessionDelta struct {
	SessionID         string
	Goal              string
	OriginalStatus    string
	NewStatus         string
	OriginalTokens    int
	NewTokens         int
	TokenDelta        int
	Drifted           bool
	OriginalFinalText string
	NewFinalText      string
	Diff              string
	ReplayDuration    time.Duration
	ReplayErr         string
}

// Report is the aggregate result of one Shadow.Run.
type Report struct {
	GeneratedAt    time.Time
	ProfileBefore  string
	ProfileAfter   string
	WorkerName     string
	DriftThreshold int
	Sessions       []SessionDelta
}

// DriftedCount is the number of sessions whose token delta crossed the
// configured threshold (or whose status changed).
func (r *Report) DriftedCount() int {
	n := 0
	for _, s := range r.Sessions {
		if s.Drifted {
			n++
		}
	}
	return n
}

// Markdown renders the report as a self-describing markdown document.
// Suitable for writing to <project>/.uta/context/shadow/<mode>-<ts>.md.
func (r *Report) Markdown() string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "# Shadow report — profile %q\n\n", r.ProfileAfter)
	fmt.Fprintf(&sb, "_Generated %s. Compared against profile %q. Drift threshold: %d tokens._\n\n",
		r.GeneratedAt.UTC().Format(time.RFC3339),
		nonEmpty(r.ProfileBefore, "(none)"),
		r.DriftThreshold)
	fmt.Fprintf(&sb, "Sessions replayed: %d. Drifted: %d.\n\n", len(r.Sessions), r.DriftedCount())
	if len(r.Sessions) == 0 {
		sb.WriteString("_No sessions matched the filter._\n")
		return sb.String()
	}
	sb.WriteString("| session | status (old → new) | tokens (old → new) | Δ | drift | replay |\n")
	sb.WriteString("|---|---|---|---|---|---|\n")
	for _, s := range r.Sessions {
		drift := " "
		if s.Drifted {
			drift = "yes"
		}
		replay := s.ReplayDuration.Round(time.Millisecond).String()
		if s.ReplayErr != "" {
			replay = "error"
		}
		fmt.Fprintf(&sb, "| `%s` | %s → %s | %d → %d | %+d | %s | %s |\n",
			shortID(s.SessionID),
			nonEmpty(s.OriginalStatus, "?"), nonEmpty(s.NewStatus, "?"),
			s.OriginalTokens, s.NewTokens, s.TokenDelta,
			drift, replay,
		)
	}
	sb.WriteString("\n---\n\n")
	for _, s := range r.Sessions {
		fmt.Fprintf(&sb, "## `%s` — %s\n\n", shortID(s.SessionID), truncate(s.Goal, 120))
		if s.ReplayErr != "" {
			fmt.Fprintf(&sb, "**replay error:** %s\n\n", s.ReplayErr)
		}
		fmt.Fprintf(&sb, "- status: `%s` → `%s`\n", nonEmpty(s.OriginalStatus, "?"), nonEmpty(s.NewStatus, "?"))
		fmt.Fprintf(&sb, "- tokens: %d → %d (Δ %+d)\n", s.OriginalTokens, s.NewTokens, s.TokenDelta)
		fmt.Fprintf(&sb, "- replay duration: %s\n\n", s.ReplayDuration.Round(time.Millisecond))
		if s.Diff != "" {
			sb.WriteString("```diff\n")
			sb.WriteString(s.Diff)
			if !strings.HasSuffix(s.Diff, "\n") {
				sb.WriteString("\n")
			}
			sb.WriteString("```\n\n")
		}
	}
	return sb.String()
}

// Run replays the source session's goal against ProfileAfter and returns
// a one-row Report. Useful as a building block for higher-level callers
// (e.g. RunMode which iterates over a mode's recent sessions).
func (sh *Shadow) Run(ctx context.Context, deps Deps) (*Report, error) {
	if sh.SourceSessionID == "" {
		return nil, errors.New("shadow: SourceSessionID is required")
	}
	if sh.ProfileAfter == nil {
		return nil, errors.New("shadow: ProfileAfter is required")
	}
	if deps.Store == nil {
		return nil, errors.New("shadow: deps.Store is required")
	}
	sess, err := deps.Store.GetSession(sh.SourceSessionID)
	if err != nil {
		return nil, fmt.Errorf("shadow: load source session: %w", err)
	}
	report := &Report{
		GeneratedAt:    time.Now(),
		ProfileAfter:   sh.ProfileAfter.Name,
		WorkerName:     firstNonEmpty(sh.WorkerName, sess.Worker),
		DriftThreshold: effectiveDriftThreshold(sh.DriftTokens, sh.ProfileAfter),
	}
	if sh.ProfileBefore != nil {
		report.ProfileBefore = sh.ProfileBefore.Name
	}
	delta := sh.replaySession(ctx, deps, sess)
	report.Sessions = append(report.Sessions, delta)
	return report, nil
}

// SelectSessions returns the sessions whose mode_name matches modeName
// and whose CreatedAt is at or after since, ordered oldest-first and
// capped at max. Exposed so callers (notably the CLI's --dry-run path)
// can enumerate the same set RunMode would replay without invoking the
// worker. since.IsZero() disables the time filter.
func SelectSessions(st *store.Store, modeName string, since time.Time, max int) ([]store.Session, error) {
	if st == nil {
		return nil, errors.New("shadow: store is required")
	}
	sessions, err := st.RecentSessionsByMode(modeName, max)
	if err != nil {
		return nil, fmt.Errorf("shadow: list sessions: %w", err)
	}
	filtered := make([]store.Session, 0, len(sessions))
	for _, s := range sessions {
		if !since.IsZero() && s.CreatedAt.Before(since) {
			continue
		}
		filtered = append(filtered, s)
	}
	// Oldest-first so the markdown reads chronologically.
	sort.Slice(filtered, func(i, j int) bool { return filtered[i].CreatedAt.Before(filtered[j].CreatedAt) })
	return filtered, nil
}

// RunMode replays every session whose mode_name equals after.Name and
// whose CreatedAt is at or after since. Sessions are ordered oldest-first
// so the report reads chronologically.
//
// before is the profile snapshot the replay is being compared against
// (purely informational on the report — Shadow never re-runs original
// sessions). after is the profile applied to every replay.
func RunMode(ctx context.Context, deps Deps, before, after *profile.MissionProfile, since time.Time, opts Options) (*Report, error) {
	if after == nil {
		return nil, errors.New("shadow: after profile is required")
	}
	if deps.Store == nil {
		return nil, errors.New("shadow: deps.Store is required")
	}
	filtered, err := SelectSessions(deps.Store, after.Name, since, opts.MaxSessions)
	if err != nil {
		return nil, err
	}

	report := &Report{
		GeneratedAt:    time.Now(),
		ProfileAfter:   after.Name,
		WorkerName:     opts.WorkerName,
		DriftThreshold: effectiveDriftThreshold(opts.DriftTokens, after),
	}
	if before != nil {
		report.ProfileBefore = before.Name
	}
	for _, sess := range filtered {
		if ctx.Err() != nil {
			break
		}
		sh := Shadow{
			SourceSessionID: sess.ID,
			ProfileBefore:   before,
			ProfileAfter:    after,
			WorkerName:      opts.WorkerName,
			Workdir:         opts.Workdir,
			Env:             opts.Env,
			DriftTokens:     opts.DriftTokens,
		}
		delta := sh.replaySession(ctx, deps, sess)
		report.Sessions = append(report.Sessions, delta)
		if opts.OnProgress != nil {
			opts.OnProgress(delta)
		}
	}
	return report, nil
}

// Options bundles the run-wide knobs for RunMode. Per-session fields
// (SourceSessionID, ProfileBefore/After) are derived inside RunMode.
type Options struct {
	WorkerName  string
	Workdir     string
	Env         []string
	DriftTokens int
	MaxSessions int
	// OnProgress is invoked after each session delta is computed. nil is
	// fine — used by the CLI to stream a progress line per session.
	OnProgress func(SessionDelta)
}

// effectiveDriftThreshold resolves the drift threshold using the standard
// override chain: explicit per-run override (e.g. a CLI flag) wins, then
// the per-profile ShadowDriftTokens setting, then DefaultDriftTokens.
func effectiveDriftThreshold(override int, p *profile.MissionProfile) int {
	if override > 0 {
		return override
	}
	if p != nil && p.ShadowDriftTokens > 0 {
		return p.ShadowDriftTokens
	}
	return DefaultDriftTokens
}

func (sh *Shadow) driftThreshold() int {
	return effectiveDriftThreshold(sh.DriftTokens, sh.ProfileAfter)
}

// replaySession is the common per-session replay routine used by both
// Run and RunMode. It loads the original final-answer blob, invokes the
// Deps.Replay function (or the default supervisor-backed replayer), and
// assembles a SessionDelta.
func (sh *Shadow) replaySession(ctx context.Context, deps Deps, sess store.Session) SessionDelta {
	delta := SessionDelta{
		SessionID:      sess.ID,
		Goal:           sess.Goal,
		OriginalStatus: sess.Status,
	}
	original := readBlobText(deps.Blobs, sess.FinalAnswerRef)
	delta.OriginalFinalText = original
	delta.OriginalTokens = countTokens(original)

	worker := firstNonEmpty(sh.WorkerName, sess.Worker)
	in := ReplayInput{
		Goal:       sess.Goal,
		WorkerName: worker,
		Profile:    sh.ProfileAfter,
		Workdir:    sh.Workdir,
		Env:        sh.Env,
	}
	replay := deps.Replay
	if replay == nil {
		replay = defaultReplay(deps)
	}
	start := time.Now()
	out, err := replay(ctx, in)
	if out.Duration == 0 {
		out.Duration = time.Since(start)
	}
	delta.ReplayDuration = out.Duration
	delta.NewFinalText = out.FinalText
	delta.NewStatus = out.Status
	delta.NewTokens = countTokens(out.FinalText)
	delta.TokenDelta = delta.NewTokens - delta.OriginalTokens
	if err != nil {
		delta.ReplayErr = err.Error()
	} else if out.Err != nil {
		delta.ReplayErr = out.Err.Error()
	}
	delta.Diff = unifiedDiff(delta.OriginalFinalText, delta.NewFinalText)
	delta.Drifted = sh.isDrifted(delta)
	return delta
}

func (sh *Shadow) isDrifted(d SessionDelta) bool {
	if d.ReplayErr != "" {
		return true
	}
	if d.OriginalStatus != d.NewStatus && d.NewStatus != "" {
		return true
	}
	if absInt(d.TokenDelta) >= sh.driftThreshold() {
		return true
	}
	return false
}

// SandboxWorkdir creates a fresh directory under os.TempDir suitable for
// use as Shadow.Workdir. When projectRoot is "" the directory is empty
// (back-compat — callers that want fixture-only sandboxes can pass "").
// When projectRoot points to a real directory, a copy of it is staged
// into the sandbox: every regular file under projectRoot is reproduced
// at the same relative path inside the temp dir, skipping VCS noise
// (.git), per-uta state (.uta/), and the temp dir itself in case the
// caller passed a parent of os.TempDir.
//
// The directory is the caller's responsibility to remove (the CLI
// defers an os.RemoveAll on it).
func SandboxWorkdir(projectRoot ...string) (string, error) {
	dir, err := os.MkdirTemp("", "uta-shadow-")
	if err != nil {
		return "", err
	}
	root := ""
	if len(projectRoot) > 0 {
		root = strings.TrimSpace(projectRoot[0])
	}
	if root == "" {
		return dir, nil
	}
	if err := copyProjectInto(root, dir); err != nil {
		os.RemoveAll(dir)
		return "", fmt.Errorf("shadow: stage sandbox from %s: %w", root, err)
	}
	return dir, nil
}

// shadowSkipDirs are paths inside projectRoot that should NOT be copied
// into the sandbox. Keeps the sandbox compact and prevents the operator
// from accidentally cloning gigabytes of node_modules or build artifacts
// just to verify a prompt change is safe.
var shadowSkipDirs = map[string]struct{}{
	".git":         {},
	".uta":         {},
	"node_modules": {},
	"dist":         {},
	"build":        {},
	".next":        {},
	"target":       {},
	"vendor":       {},
}

// copyProjectInto duplicates the regular files under src into dst,
// skipping VCS and per-uta state. Symlinks are dereferenced; broken or
// circular ones are skipped. Errors that affect a single file are
// logged via fmt to stderr-equivalent (return wrap) so the caller can
// decide whether to abort — for shadow the policy is best-effort.
func copyProjectInto(src, dst string) error {
	srcAbs, err := filepath.Abs(src)
	if err != nil {
		return err
	}
	return filepath.WalkDir(srcAbs, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(srcAbs, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		// Skip top-level + nested matches of any blocked directory name.
		parts := strings.Split(filepath.ToSlash(rel), "/")
		for _, p := range parts {
			if _, skip := shadowSkipDirs[p]; skip {
				if d.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		// Resolve symlinks; skip broken ones.
		info, err := os.Stat(path)
		if err != nil {
			return nil
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		return copyFile(path, target, info.Mode().Perm())
	})
}

// copyFile streams src to dst with the given mode. Caller is responsible
// for ensuring dst's parent exists.
func copyFile(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// defaultReplay wires a real engine.Supervisor against a throwaway
// SQLite store + blob dir, so replays never write to the operator's
// production session history.
func defaultReplay(deps Deps) ReplayFunc {
	return func(ctx context.Context, in ReplayInput) (ReplayOutput, error) {
		if deps.Registry == nil {
			return ReplayOutput{}, errors.New("shadow: deps.Registry is required for the default replayer")
		}
		tmp, err := os.MkdirTemp("", "uta-shadow-store-")
		if err != nil {
			return ReplayOutput{}, fmt.Errorf("shadow: tempdir: %w", err)
		}
		defer os.RemoveAll(tmp)
		st, err := store.Open(filepath.Join(tmp, "shadow.db"))
		if err != nil {
			return ReplayOutput{}, fmt.Errorf("shadow: open temp store: %w", err)
		}
		defer st.Close()
		bus := trajectory.NewBus()
		defer bus.Shutdown()
		recorder := trajectory.NewRecorder(st)
		sup := engine.New(engine.Deps{
			Store:    st,
			Blobs:    store.NewBlobs(tmp),
			Recorder: recorder,
			Bus:      bus,
			Registry: deps.Registry,
		})
		req := engine.RunRequest{
			Goal:       in.Goal,
			WorkerName: in.WorkerName,
			Workdir:    in.Workdir,
			Env:        in.Env,
		}
		engine.ApplyProfile(&req, in.Profile)
		start := time.Now()
		res, runErr := sup.Run(ctx, req)
		out := ReplayOutput{
			FinalText: res.FinalAnswer,
			Status:    res.Status,
			Duration:  time.Since(start),
		}
		if runErr != nil {
			out.Err = runErr
		}
		return out, nil
	}
}

// readBlobText returns the bytes at ref as a string. Missing or empty
// refs return "" so Shadow can compare against pre-blob-era sessions
// without erroring.
func readBlobText(_ *store.Blobs, ref string) string {
	if ref == "" {
		return ""
	}
	f, err := os.Open(ref)
	if err != nil {
		return ""
	}
	defer f.Close()
	data, err := io.ReadAll(f)
	if err != nil {
		return ""
	}
	return string(data)
}

// countTokens is a deterministic, dependency-free approximation of LLM
// token count: whitespace-separated word count. Good enough to flag
// "this response got way longer/shorter" without pulling in a tokenizer.
func countTokens(s string) int {
	if s == "" {
		return 0
	}
	n := 0
	inWord := false
	for _, r := range s {
		switch r {
		case ' ', '\t', '\n', '\r', '\f', '\v':
			inWord = false
		default:
			if !inWord {
				n++
				inWord = true
			}
		}
	}
	return n
}

// unifiedDiff returns a tiny, dependency-free line-based diff suitable
// for embedding in markdown. Not a real Myers diff — it simply walks the
// two slices in lockstep and emits +/- lines for any differences. Good
// enough to make the report self-describing without a third-party dep.
func unifiedDiff(a, b string) string {
	if a == b {
		return ""
	}
	la := strings.Split(strings.TrimRight(a, "\n"), "\n")
	lb := strings.Split(strings.TrimRight(b, "\n"), "\n")
	var sb strings.Builder
	i, j := 0, 0
	for i < len(la) || j < len(lb) {
		switch {
		case i < len(la) && j < len(lb) && la[i] == lb[j]:
			sb.WriteString("  " + la[i] + "\n")
			i++
			j++
		case i < len(la) && (j >= len(lb) || la[i] != lb[j]):
			sb.WriteString("- " + la[i] + "\n")
			i++
		case j < len(lb):
			sb.WriteString("+ " + lb[j] + "\n")
			j++
		}
	}
	return sb.String()
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func nonEmpty(a, fallback string) string {
	if a != "" {
		return a
	}
	return fallback
}

func absInt(n int) int {
	if n < 0 {
		return -n
	}
	return n
}

func shortID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if n <= 0 || len(s) <= n {
		return s
	}
	if n > 1 {
		return s[:n-1] + "…"
	}
	return s[:n]
}
