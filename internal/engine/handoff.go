package engine

import (
	"bufio"
	"context"
	"errors"
	"os/exec"
	"strings"

	"github.com/unleashtheagents/uta/internal/profile"
	"github.com/unleashtheagents/uta/internal/trajectory"
)

// ProfileResolver looks up a MissionProfile by name. The caller supplies
// this so the engine doesn't have to know where profiles live on disk.
// Returning (nil, nil) is treated as "not found" and surfaces a clean
// trajectory warning instead of an error.
type ProfileResolver func(name string) (*profile.MissionProfile, error)

// ChangedFilesFunc returns the list of files modified by a run, used to
// evaluate Handoff conditions. Typically backed by `git diff --name-only`
// against the workdir's pre-run HEAD. Returning (nil, nil) is fine: a
// Handoff whose Condition is empty will still fire, while one that
// requires files won't.
type ChangedFilesFunc func(ctx context.Context, sessionID string) ([]string, error)

// HandoffMatch is the resolved outcome of evaluating a profile's
// OnComplete list against the changes from a session. Nil when no entry
// matches (or no OnComplete is configured).
type HandoffMatch struct {
	Handoff      profile.Handoff
	TargetMode   *profile.MissionProfile
	ChangedFiles []string
	Prompt       string
}

// EvaluateHandoffs walks the supplied handoff list in declaration order
// and returns the first one whose Condition matches `changedFiles` AND
// whose TargetMode resolves via `resolve`. Handoffs whose target is
// missing emit a HandoffSkipped trajectory event but do not abort the
// walk — a profile can declare a "fallback" target after a primary one
// that may not exist on every machine.
//
// Returns (nil, nil) when no handoff matches; the caller should treat
// that as "nothing to chain" rather than an error.
func (s *Supervisor) EvaluateHandoffs(
	priorSessionID string,
	handoffs []profile.Handoff,
	changedFiles []string,
	resolve ProfileResolver,
) (*HandoffMatch, error) {
	if len(handoffs) == 0 {
		return nil, nil
	}
	for i := range handoffs {
		h := handoffs[i]
		if !h.Condition.Matches(changedFiles) {
			s.emit(priorSessionID, "", trajectory.HandoffSkipped, map[string]any{
				"target_mode":   h.TargetMode,
				"reason":        "condition not satisfied",
				"files_changed": len(changedFiles),
			})
			continue
		}
		var (
			target *profile.MissionProfile
			err    error
		)
		if resolve != nil {
			target, err = resolve(h.TargetMode)
		}
		if err != nil || target == nil {
			reason := "target mode not found"
			if err != nil {
				reason = err.Error()
			}
			s.emit(priorSessionID, "", trajectory.HandoffSkipped, map[string]any{
				"target_mode": h.TargetMode,
				"reason":      reason,
			})
			continue
		}
		return &HandoffMatch{
			Handoff:      h,
			TargetMode:   target,
			ChangedFiles: changedFiles,
			Prompt:       h.RenderPrompt(priorSessionID),
		}, nil
	}
	return nil, nil
}

// EmitHandoffStarted records the start of a chained run on the prior
// session. The chained run gets its own GoalReceived (with handoff_from
// in the payload) when the supervisor processes the follow-up
// RunRequest; this event closes the loop on the upstream session so
// `uta trajectory` shows the handoff point in chronological order.
func (s *Supervisor) EmitHandoffStarted(priorSessionID string, m *HandoffMatch) {
	if m == nil {
		return
	}
	s.emit(priorSessionID, "", trajectory.HandoffStarted, map[string]any{
		"target_mode":   m.Handoff.TargetMode,
		"files_changed": len(m.ChangedFiles),
		"prompt_chars":  len(m.Prompt),
	})
}

// EmitHandoffCompleted records the terminal state of a chained run on
// the prior session, linking back to the new session id and surfacing
// its status. Use trajectory.HandoffCancelled instead when the chain was
// interrupted (Ctrl-C); EmitHandoffCancelled is a convenience wrapper.
func (s *Supervisor) EmitHandoffCompleted(priorSessionID, chainedSessionID, status string) {
	s.emit(priorSessionID, "", trajectory.HandoffCompleted, map[string]any{
		"chained_session_id": chainedSessionID,
		"status":             status,
	})
}

// EmitHandoffCancelled records a chained run that was interrupted (e.g.
// the user hit Ctrl-C between the dev run and the audit run). The
// prior session stays "completed"; this event is the only trace that a
// follow-up was scheduled but not finished.
func (s *Supervisor) EmitHandoffCancelled(priorSessionID, targetMode, reason string) {
	s.emit(priorSessionID, "", trajectory.HandoffCancelled, map[string]any{
		"target_mode": targetMode,
		"reason":      reason,
	})
}

// GitChangedFiles is the default ChangedFilesFunc implementation: shells
// out to `git diff --name-only HEAD` in workdir and returns the list of
// modified paths (staged + unstaged + untracked, separately). Returns
// (nil, nil) when git is unavailable or the workdir isn't a git repo —
// callers should treat that as "no change detection available" rather
// than failing the chain. Real failures (index lock, permission denied,
// git crashes) surface as a non-nil error so callers can distinguish
// "nothing to detect" from "detection broke".
func GitChangedFiles(ctx context.Context, workdir string) ([]string, error) {
	if workdir == "" {
		return nil, nil
	}
	if _, err := exec.LookPath("git"); err != nil {
		return nil, nil
	}
	// Combine tracked-but-modified and untracked files. `git status
	// --porcelain` gives both in one shot; the leading two columns are
	// status codes, then a space, then the path.
	cmd := exec.CommandContext(ctx, "git", "-C", workdir, "status", "--porcelain")
	out, err := cmd.Output()
	if err != nil {
		// Only the "not a git repository" outcome is treated as a clean
		// "no change detection available" signal. Other ExitErrors (index
		// lock contention, permission denied on .git, internal git
		// failures) carry their stderr back to the caller so the chain
		// doesn't silently mis-evaluate handoff conditions on a partial
		// or empty file list.
		var ee *exec.ExitError
		if errors.As(err, &ee) && isNotAGitRepo(ee.Stderr) {
			return nil, nil
		}
		return nil, err
	}
	var files []string
	sc := bufio.NewScanner(strings.NewReader(string(out)))
	for sc.Scan() {
		line := sc.Text()
		if len(line) < 4 {
			continue
		}
		// Porcelain v1: "XY <path>" or "XY <orig> -> <path>" for renames.
		// We only care about the destination path.
		path := strings.TrimSpace(line[3:])
		if idx := strings.Index(path, " -> "); idx >= 0 {
			path = path[idx+4:]
		}
		// Strip surrounding quotes for paths with shell-significant chars.
		path = strings.Trim(path, "\"")
		if path != "" {
			files = append(files, path)
		}
	}
	return files, nil
}

// isNotAGitRepo recognizes git's exit-128 stderr for "this directory is
// not (or no longer) a git working tree". Git localizes very few of its
// fatal messages, so substring match on the English form is reliable in
// practice; the bare-repo variant ("this operation must be run in a
// work tree") is also treated as no-detection-available since `git
// status --porcelain` produces nothing useful there either.
func isNotAGitRepo(stderr []byte) bool {
	s := string(stderr)
	return strings.Contains(s, "not a git repository") ||
		strings.Contains(s, "this operation must be run in a work tree")
}
