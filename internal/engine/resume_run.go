// resume_run.go — re-enter a crashed or cancelled run.
//
// A multi-subtask run that dies 4 subtasks into 6 has already paid for
// those 4 results; they're persisted (subtask rows + prompt blobs).
// BuildResumeRunRequest recovers that state into a RunRequest that
// re-executes ONLY the unfinished subtasks while feeding the completed
// outcomes into synthesis — so the final answer covers the whole
// original plan without re-paying for finished work.
//
// The re-entry runs as a NEW session (clean trajectory, clean budget)
// linked back via meta_json.resumed_from_session. This is distinct from
// Supervisor.Resume, which is a conversational follow-up turn on a
// COMPLETED session; resume-run is crash recovery for an UNFINISHED one.
package engine

import (
	"bytes"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/unleashtheagents/uta/internal/store"
)

// ResumeRunInfo summarizes what BuildResumeRunRequest recovered, so the
// CLI can print "skipping 4 completed, re-running 2" before dispatch.
type ResumeRunInfo struct {
	PriorSessionID string
	PriorStatus    string
	Completed      int // outcomes recovered, will not re-run
	Rerun          int // subtasks that will execute again
}

// BuildResumeRunRequest reads the prior session's plan back out of the
// store and splits it: completed subtasks become PriorOutcomes,
// everything else (pending / running-at-crash / failed) becomes
// PreSetSubtasks for re-execution. Prompts are recovered from the
// prompt_ref blobs.
//
// Returns an error when the session doesn't exist, is already
// completed (use `uta resume` for follow-up turns), or has no
// recoverable subtasks.
//
// The returned request carries the prior session's goal, worker, and
// mode; callers layer their own flags and profile preamble on top
// exactly as they would for a fresh run.
func BuildResumeRunRequest(st *store.Store, blobs *store.Blobs, sessionID string) (RunRequest, ResumeRunInfo, error) {
	var info ResumeRunInfo
	sess, err := st.GetSession(sessionID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return RunRequest{}, info, fmt.Errorf("session not found: %s", sessionID)
		}
		return RunRequest{}, info, err
	}
	if sess.Status == "completed" {
		return RunRequest{}, info, fmt.Errorf(
			"session %s already completed — use `uta resume %s -g \"...\"` for a follow-up turn",
			sessionID, sessionID)
	}
	subs, err := st.SubtaskListBySession(sessionID, 0, 0)
	if err != nil {
		return RunRequest{}, info, fmt.Errorf("read subtasks: %w", err)
	}
	if len(subs) == 0 {
		return RunRequest{}, info, fmt.Errorf(
			"session %s has no recorded subtasks (it died before planning finished) — re-run the goal with `uta run`",
			sessionID)
	}

	info.PriorSessionID = sessionID
	info.PriorStatus = sess.Status

	var prior []SubtaskOutcome
	var rerun []SubtaskSpec
	for _, sub := range subs {
		// "resume turn" rows (from Supervisor.Resume) and synthesis rows
		// have no spec id and aren't plan members; skip defensively.
		specID := sub.SpecID
		if specID == "" {
			specID = sub.ID
		}
		switch sub.Status {
		case "completed", "ok", "done":
			prior = append(prior, SubtaskOutcome{
				ID:     specID,
				Title:  sub.Title,
				Worker: sub.Worker,
				Result: sub.ResultText,
			})
			info.Completed++
		default:
			prompt, perr := readPromptBlob(blobs, sub.PromptRef)
			if perr != nil || strings.TrimSpace(prompt) == "" {
				// Prompt blob lost — cannot re-run this one faithfully.
				// Surface it as a failed prior outcome so synthesis at
				// least knows the gap exists.
				prior = append(prior, SubtaskOutcome{
					ID:     specID,
					Title:  sub.Title,
					Worker: sub.Worker,
					Result: "subtask could not be recovered for re-run (prompt blob missing)",
					Failed: true,
				})
				continue
			}
			rerun = append(rerun, SubtaskSpec{
				ID:     specID,
				Title:  sub.Title,
				Prompt: prompt,
				Worker: sub.Worker,
			})
			info.Rerun++
		}
	}
	if info.Rerun == 0 {
		return RunRequest{}, info, fmt.Errorf(
			"session %s has no unfinished subtasks to re-run (%d completed) — synthesis may have been the failure; use `uta resume` instead",
			sessionID, info.Completed)
	}

	req := RunRequest{
		Goal:               sess.Goal,
		WorkerName:         sess.Worker,
		PlannerName:        sess.Planner,
		ModeName:           sess.ModeName,
		PreSetSubtasks:     rerun,
		PriorOutcomes:      prior,
		ResumedFromSession: sessionID,
	}
	return req, info, nil
}

// readPromptBlob loads a prompt blob's text by ref. Empty ref is not an
// error here — the caller decides what a missing prompt means.
func readPromptBlob(blobs *store.Blobs, ref string) (string, error) {
	if strings.TrimSpace(ref) == "" {
		return "", errors.New("empty prompt ref")
	}
	var buf bytes.Buffer
	if err := blobs.Get(ref, &buf); err != nil {
		return "", err
	}
	return buf.String(), nil
}
