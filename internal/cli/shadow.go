package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/unleashtheagents/uta/internal/profile"
	"github.com/unleashtheagents/uta/internal/shadow"
)

// newShadowCmd registers `uta shadow run` — regression-tests prompt
// changes by replaying historical sessions against the current profile.
func newShadowCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "shadow",
		Short: "regression-test prompt changes by replaying past sessions",
		Long: `Shadow runs replay the goals of past sessions against the current
MissionProfile so you can spot when a profile / persona / vibe edit
materially changes output. Each replay happens in an isolated workdir
and writes to a throwaway store — your production session history is
never modified.`,
	}
	cmd.AddCommand(newShadowRunCmd())
	return cmd
}

func newShadowRunCmd() *cobra.Command {
	var (
		modeName    string
		since       string
		maxSessions int
		driftTokens int
		worker      string
		workdir     string
		outPath     string
		dryRun      bool
	)
	cmd := &cobra.Command{
		Use:   "run",
		Short: "replay sessions for a mode and write a drift report",
		Long: `Picks recent sessions whose mode_name matches --mode and whose
created_at is within --since (e.g. "7d", "24h", "30m"). Each session's
original goal is replayed against the named MissionProfile, the new
output is diffed against the original, and a markdown report is written
to <project>/.uta/context/shadow/<mode>-<YYYYMMDD-HHMMSS>.md.

The exit code is 0 when no session drifted past the --drift-tokens
threshold; non-zero when at least one session changed materially.

--dry-run lists which sessions WOULD be replayed without invoking any
provider — useful for sanity-checking the filter.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if strings.TrimSpace(modeName) == "" {
				return errors.New("--mode is required")
			}
			window, err := parseSinceDuration(since)
			if err != nil {
				return err
			}

			app, err := newApp(cmd.Context())
			if err != nil {
				return err
			}
			defer app.Close()

			profiles, perrs := profile.LoadAll(app.GlobalHome, app.ProjectRoot)
			for _, e := range perrs {
				fmt.Fprintln(cmd.ErrOrStderr(), "warn: profile:", e)
			}
			after, err := profile.Find(profiles, modeName)
			if err != nil {
				return err
			}

			cutoff := time.Time{}
			if window > 0 {
				cutoff = time.Now().Add(-window)
			}

			filtered, err := shadow.SelectSessions(app.Store, modeName, cutoff, effectiveMaxShadow(maxSessions))
			if err != nil {
				return err
			}

			if dryRun {
				fmt.Fprintf(cmd.OutOrStdout(),
					"[shadow] dry-run: mode=%q since=%s → %d session(s) match\n",
					modeName, window, len(filtered))
				for _, s := range filtered {
					fmt.Fprintf(cmd.OutOrStdout(), "  %s\n", s.ID)
				}
				return nil
			}

			if len(filtered) == 0 {
				fmt.Fprintf(cmd.ErrOrStderr(),
					"[shadow] no sessions matched mode=%q since=%s\n", modeName, window)
				return nil
			}

			if worker == "" {
				// Default to whatever the most recent matching session used
				// so the replay does not require an explicit --worker on
				// every invocation. filtered is oldest-first, so the last
				// element is the freshest. Falls back to the registry's
				// first available provider when that session's worker is
				// empty.
				worker = filtered[len(filtered)-1].Worker
			}
			if worker == "" {
				det := app.Registry.DetectAll(cmd.Context())
				avail := availableProviders(app.Registry.Names(), det)
				if len(avail) == 0 {
					return errNoProviders
				}
				worker = avail[0]
			}
			if _, ok := app.Registry.Get(worker); !ok {
				return fmt.Errorf("worker %q not registered", worker)
			}

			sandbox := workdir
			cleanupSandbox := func() {}
			if sandbox == "" {
				// Stage a copy of the project tree so replays can read the
				// real files (a previously-empty sandbox only made sense
				// for tests). Outside a project, fall back to an empty
				// tempdir — same back-compat as before.
				stageRoot := ""
				if app.InProject() {
					stageRoot = app.ProjectRoot
				}
				dir, err := shadow.SandboxWorkdir(stageRoot)
				if err != nil {
					return fmt.Errorf("create sandbox: %w", err)
				}
				sandbox = dir
				cleanupSandbox = func() { os.RemoveAll(dir) }
			}
			defer cleanupSandbox()

			ctx, cancel := signalContext(cmd.Context())
			defer cancel()

			env := []string{}
			if app.InProject() {
				env = append(env,
					"UTA_PROJECT_ROOT="+app.ProjectRoot,
					"UTA_CONTEXT_DIR="+app.ContextDir,
					"UTA_PROJECT_NAME="+app.ProjectName,
				)
			}

			deps := shadow.Deps{
				Store:    app.Store,
				Blobs:    app.Blobs,
				Registry: app.Registry,
			}
			opts := shadow.Options{
				WorkerName:  worker,
				Workdir:     sandbox,
				Env:         env,
				DriftTokens: driftTokens,
				MaxSessions: effectiveMaxShadow(maxSessions),
				OnProgress: func(d shadow.SessionDelta) {
					flag := " "
					if d.Drifted {
						flag = "!"
					}
					fmt.Fprintf(cmd.ErrOrStderr(),
						"[shadow] %s %s tokens %d→%d (Δ%+d) status %s→%s\n",
						flag, shortShadowID(d.SessionID),
						d.OriginalTokens, d.NewTokens, d.TokenDelta,
						nonEmptyShadow(d.OriginalStatus, "?"),
						nonEmptyShadow(d.NewStatus, "?"),
					)
				},
			}
			// ProfileBefore: pass the same loaded profile as the baseline
			// label. The original sessions ran under ModeName=after.Name;
			// at this version that's the closest meaningful baseline. A
			// future commit can snapshot the profile body at run time so
			// drift over edits is detectable; for now the field stops
			// reading "(none)" in the report.
			report, err := shadow.RunMode(ctx, deps, after, after, cutoff, opts)
			if err != nil {
				return err
			}

			path, werr := writeShadowReport(app.ProjectRoot, modeName, outPath, report.Markdown())
			if werr != nil {
				fmt.Fprintln(cmd.ErrOrStderr(), "warn: write report:", werr)
			} else if path != "" {
				fmt.Fprintf(cmd.ErrOrStderr(), "[shadow] report written to %s\n", path)
			}

			fmt.Fprintf(cmd.OutOrStdout(),
				"[shadow] mode=%q replayed=%d drifted=%d threshold=%d\n",
				modeName, len(report.Sessions), report.DriftedCount(), report.DriftThreshold)

			if report.DriftedCount() > 0 {
				return exitWith(5)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&modeName, "mode", "", "MissionProfile name to replay sessions for (required)")
	cmd.Flags().StringVar(&since, "since", "7d", "look-back window: e.g. '7d', '24h', '30m'. '' means no time filter.")
	cmd.Flags().IntVar(&maxSessions, "max-sessions", 50, "hard cap on sessions to replay (default 50)")
	cmd.Flags().IntVar(&driftTokens, "drift-tokens", 0,
		fmt.Sprintf("per-session token delta at which a replay is flagged as drifted (0 = use profile's shadow_drift_tokens or %d)", shadow.DefaultDriftTokens))
	cmd.Flags().StringVar(&worker, "worker", "", "worker provider for replays (defaults to the original session's worker)")
	cmd.Flags().StringVar(&workdir, "workdir", "", "sandbox workdir for replays (defaults to a temp dir auto-cleaned on exit)")
	cmd.Flags().StringVar(&outPath, "out", "", "explicit report path. Defaults to <project>/.uta/context/shadow/<mode>-<ts>.md, or '-' for stdout.")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "list matching sessions without invoking the worker")
	return cmd
}

// parseSinceDuration accepts both Go duration strings ("24h", "30m") and
// the common shorthand "<n>d" / "<n>w" since neither is in time.ParseDuration.
// An empty string returns 0 (no time filter).
func parseSinceDuration(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, nil
	}
	// Day / week shorthand.
	if n, unit, ok := splitDurationShorthand(s); ok {
		switch unit {
		case "d":
			return time.Duration(n) * 24 * time.Hour, nil
		case "w":
			return time.Duration(n) * 7 * 24 * time.Hour, nil
		}
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("--since: %w (try '24h', '7d', '2w', '30m')", err)
	}
	return d, nil
}

func splitDurationShorthand(s string) (int, string, bool) {
	if len(s) < 2 {
		return 0, "", false
	}
	unit := s[len(s)-1:]
	if unit != "d" && unit != "w" {
		return 0, "", false
	}
	n, err := strconv.Atoi(s[:len(s)-1])
	if err != nil || n < 0 {
		return 0, "", false
	}
	return n, unit, true
}

// writeShadowReport persists the markdown report and returns the absolute
// path it wrote (or "" when out=="-", meaning stdout). When out is "" the
// canonical path under <project>/.uta/context/shadow/ is used. When the
// caller is not in a project AND out is empty, the report is written to a
// temp file so the operator always has a discoverable artifact.
func writeShadowReport(projectRoot, mode, out, body string) (string, error) {
	if out == "-" {
		fmt.Print(body)
		return "", nil
	}
	if out == "" {
		ts := time.Now().UTC().Format("20060102-150405")
		name := fmt.Sprintf("%s-%s.md", sanitizeShadowMode(mode), ts)
		if projectRoot != "" {
			out = filepath.Join(projectRoot, ".uta", "context", "shadow", name)
		} else {
			out = filepath.Join(os.TempDir(), name)
		}
	}
	if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
		return "", err
	}
	if err := os.WriteFile(out, []byte(body), 0o644); err != nil {
		return "", err
	}
	return out, nil
}

func sanitizeShadowMode(s string) string {
	if s == "" {
		return "shadow"
	}
	var sb strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			sb.WriteRune(r)
		default:
			sb.WriteRune('-')
		}
	}
	return sb.String()
}

func effectiveMaxShadow(n int) int {
	if n <= 0 {
		return 50
	}
	return n
}

func shortShadowID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

func nonEmptyShadow(s, fallback string) string {
	if s != "" {
		return s
	}
	return fallback
}
