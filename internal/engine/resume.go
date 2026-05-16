package engine

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/unleashtheagents/uta/internal/provider"
	"github.com/unleashtheagents/uta/internal/store"
	"github.com/unleashtheagents/uta/internal/trajectory"
)

// ResumeRequest continues a prior orchestration: a new uta session is created
// but the underlying worker call goes to Resumable.ResumeHeadless against the
// provider session id captured on the prior run's last subtask. There is no
// planner step and no synthesis — this is one-shot follow-up.
type ResumeRequest struct {
	PriorSessionID  string
	Goal            string
	WorkerName      string // optional override; defaults to prior session's worker
	SubtaskTimeout  time.Duration
	RunTimeout      time.Duration
	PreApproveTools []string
	Workdir         string
}

// Resume executes a follow-up turn. Returns the new session's RunResult.
func (s *Supervisor) Resume(ctx context.Context, req ResumeRequest) (RunResult, error) {
	if strings.TrimSpace(req.Goal) == "" {
		return RunResult{}, errors.New("--goal is required for resume")
	}
	prior, err := s.deps.Store.GetSession(req.PriorSessionID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return RunResult{}, fmt.Errorf("prior session not found: %s", req.PriorSessionID)
		}
		return RunResult{}, err
	}
	last, err := s.deps.Store.LastSubtask(req.PriorSessionID)
	if err != nil {
		return RunResult{}, fmt.Errorf("read prior subtask: %w", err)
	}
	if last.ProviderSessionID == "" {
		return RunResult{}, errors.New("prior session has no recorded provider session id; can't resume")
	}

	workerName := req.WorkerName
	if workerName == "" {
		workerName = prior.Worker
	}
	prov, ok := s.deps.Registry.Get(workerName)
	if !ok {
		return RunResult{}, fmt.Errorf("worker %q not registered", workerName)
	}
	resumable, ok := prov.(provider.Resumable)
	if !ok {
		return RunResult{}, fmt.Errorf("provider %q does not support resume", workerName)
	}

	if req.SubtaskTimeout <= 0 {
		req.SubtaskTimeout = 10 * time.Minute
	}
	if req.RunTimeout <= 0 {
		req.RunTimeout = 30 * time.Minute
	}

	ctx, cancel := context.WithTimeout(ctx, req.RunTimeout)
	defer cancel()

	newSession := uuid.NewString()
	meta, _ := json.Marshal(map[string]string{"resumed_from": req.PriorSessionID})
	if err := s.deps.Store.CreateSession(store.Session{
		ID:        newSession,
		Goal:      req.Goal,
		Worker:    workerName,
		Status:    "running",
		CreatedAt: time.Now(),
		MetaJSON:  string(meta),
	}); err != nil {
		return RunResult{}, err
	}
	s.emit(newSession, "", trajectory.GoalReceived, map[string]any{
		"goal": req.Goal, "worker": workerName, "resumed_from": req.PriorSessionID,
		"prior_provider_session_id": last.ProviderSessionID,
	})

	subtaskID := uuid.NewString()
	promptRef, _ := s.deps.Blobs.Put([]byte(req.Goal), "txt")
	startedAt := time.Now()
	if err := s.deps.Store.CreateSubtask(store.Subtask{
		ID: subtaskID, SessionID: newSession, Ord: 0,
		Title: "resume turn", PromptRef: promptRef, Worker: workerName, Status: "running",
	}); err != nil {
		return RunResult{}, err
	}
	s.emit(newSession, subtaskID, trajectory.SubtaskStarted, map[string]any{
		"title": "resume turn", "worker": workerName, "resume_id": last.ProviderSessionID,
	})

	subCtx, subCancel := context.WithTimeout(ctx, req.SubtaskTimeout)
	defer subCancel()

	opts := provider.RunOptions{
		Workdir:         req.Workdir,
		Timeout:         req.SubtaskTimeout,
		PreApproveTools: req.PreApproveTools,
	}
	events := make(chan provider.Event, 64)
	drainDone := make(chan struct{})
	go func() {
		for ev := range events {
			s.publishProviderEvent(newSession, subtaskID, ev)
		}
		close(drainDone)
	}()
	result, callErr := resumable.ResumeHeadless(subCtx, last.ProviderSessionID, req.Goal, opts, events)
	close(events)
	<-drainDone
	completedAt := time.Now()

	rawRef := ""
	if len(result.RawOutput) > 0 {
		if r, perr := s.deps.Blobs.Put(result.RawOutput, providerBlobExt(workerName)); perr == nil {
			rawRef = r
		}
	}

	if callErr != nil {
		kind := classifyError(callErr)
		s.emit(newSession, subtaskID, trajectory.SubtaskFailed, map[string]any{"error": callErr.Error(), "kind": kind})
		_ = s.deps.Store.UpdateSubtask(store.Subtask{
			ID: subtaskID, ProviderSessionID: result.SessionID, Status: "failed",
			StartedAt: &startedAt, CompletedAt: &completedAt,
			ResultText: truncate(result.FinalText, 8000), RawOutputRef: rawRef,
			Error: callErr.Error(), ErrorKind: kind,
		})
		s.emit(newSession, "", trajectory.RunFailed, map[string]any{"reason": callErr.Error()})
		_ = s.deps.Store.MarkSession(newSession, "failed", "")
		return RunResult{SessionID: newSession, Status: "failed"}, callErr
	}

	s.emit(newSession, subtaskID, trajectory.SubtaskCompleted, map[string]any{
		"chars": len(result.FinalText), "provider_session_id": result.SessionID,
	})
	_ = s.deps.Store.UpdateSubtask(store.Subtask{
		ID: subtaskID, ProviderSessionID: result.SessionID, Status: "completed",
		StartedAt: &startedAt, CompletedAt: &completedAt,
		ResultText: truncate(result.FinalText, 8000), RawOutputRef: rawRef,
	})
	finalRef, _ := s.deps.Blobs.Put([]byte(result.FinalText), "txt")
	_ = s.deps.Store.MarkSession(newSession, "completed", finalRef)
	s.emit(newSession, "", trajectory.RunCompleted, map[string]any{"status": "completed", "subtasks": 1})

	return RunResult{
		SessionID:   newSession,
		Status:      "completed",
		FinalAnswer: result.FinalText,
	}, nil
}
