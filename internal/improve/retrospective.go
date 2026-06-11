// Retrospective scheduling for MissionProfiles. After every session run
// against a mode whose profile sets retrospective_every: N, the orchestrator
// calls MaybeRetrospective. Every Nth completed session triggers a
// per-mode synthesis: the configured worker is handed a prompt summarizing
// the last N sessions (their goals, statuses, subtask titles + results) and
// asked to produce a LESSONS_LEARNED-style markdown. The result is written
// to <project>/.uta/context/retrospectives/<mode>-<YYYYMMDD>.md.
//
// The lessons file is human-curated input — uta itself does NOT
// auto-apply lessons as feedback memory. That decision belongs to the
// operator who reads the markdown.
package improve

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/unleashtheagents/uta/internal/provider"
	"github.com/unleashtheagents/uta/internal/store"
	"github.com/unleashtheagents/uta/internal/trajectory"
)

// RetroDeps is the wired infrastructure MaybeRetrospective needs.
type RetroDeps struct {
	Store    *store.Store
	Registry *provider.Registry
	Recorder *trajectory.Recorder // optional; nil = skip trajectory writes
	Bus      *trajectory.Bus      // optional; nil = skip live publish
}

// RetroRequest configures one cadence check. When Every is 0, ModeName is
// empty, or ProjectRoot is empty, MaybeRetrospective is a no-op.
type RetroRequest struct {
	ModeName       string
	ProjectRoot    string
	WorkerName     string
	Every          int
	PromptTemplate string
	Workdir        string
	Env            []string
	Timeout        time.Duration

	// PriorSessionID is the session whose completion triggered the cadence
	// check. retrospective_started / retrospective_completed trajectory
	// events are attributed to this session so the operator can correlate
	// the retrospective with the run that produced it.
	PriorSessionID string
}

// RetroResult summarizes a MaybeRetrospective outcome.
type RetroResult struct {
	// Triggered is true when the cadence boundary was hit and a synthesis
	// run was attempted. False on disabled mode, off-cadence, or missing
	// project root.
	Triggered bool
	// Path is the absolute path to the written markdown file. Empty when
	// the run was skipped or the provider call failed before write.
	Path string
	// SessionCount is the total sessions for the mode at the time of the
	// check (post-increment). Always populated when ModeName is non-empty.
	SessionCount int
	// Summarized is the number of recent sessions included in the synthesis
	// prompt. Always equals min(Every, available sessions).
	Summarized int
}

// MaybeRetrospective inspects the session count for req.ModeName and, when
// it is a positive multiple of req.Every, runs a synthesis pass that writes
// a retrospective markdown under <ProjectRoot>/.uta/context/retrospectives/.
// All disable conditions (Every == 0, ModeName == "", missing project root,
// off-cadence) are signalled by a non-nil result with Triggered=false and a
// nil error. A provider failure DOES surface as an error so the caller can
// decide whether to log it.
func MaybeRetrospective(ctx context.Context, deps RetroDeps, req RetroRequest) (*RetroResult, error) {
	if req.ModeName == "" || req.Every <= 0 || req.ProjectRoot == "" {
		return &RetroResult{}, nil
	}
	if deps.Store == nil || deps.Registry == nil {
		return nil, errors.New("retrospective: deps.Store and deps.Registry are required")
	}

	count, err := deps.Store.CountSessionsByMode(req.ModeName)
	if err != nil {
		return nil, fmt.Errorf("retrospective: count sessions: %w", err)
	}
	res := &RetroResult{SessionCount: count}
	if count <= 0 || count%req.Every != 0 {
		return res, nil
	}

	recent, err := deps.Store.RecentSessionsByMode(req.ModeName, req.Every)
	if err != nil {
		return nil, fmt.Errorf("retrospective: list sessions: %w", err)
	}
	res.Summarized = len(recent)
	if res.Summarized == 0 {
		return res, nil
	}

	prov, ok := deps.Registry.Get(req.WorkerName)
	if !ok {
		return nil, fmt.Errorf("retrospective: provider %q not registered", req.WorkerName)
	}

	res.Triggered = true

	timeout := req.Timeout
	if timeout <= 0 {
		timeout = 10 * time.Minute
	}

	emitRetro(deps, req.PriorSessionID, trajectory.RetrospectiveStarted, map[string]any{
		"mode":                req.ModeName,
		"session_count":       count,
		"summarized_sessions": res.Summarized,
		"worker":              req.WorkerName,
		"every":               req.Every,
	})

	prompt := renderRetroPrompt(req.ModeName, req.PromptTemplate, recent, deps.Store)

	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	events := make(chan provider.Event, 32)
	drainDone := make(chan struct{})
	go func() {
		// Drain to prevent provider deadlock; retrospective events are
		// intentionally NOT republished on the trajectory bus — the
		// retrospective is a side-channel synthesis, not a session.
		for range events {
		}
		close(drainDone)
	}()

	result, runErr := prov.RunHeadless(runCtx, prompt, provider.RunOptions{
		Workdir: req.Workdir,
		Env:     req.Env,
		Timeout: timeout,
	}, events)
	close(events)
	<-drainDone

	if runErr != nil {
		emitRetro(deps, req.PriorSessionID, trajectory.RetrospectiveCompleted, map[string]any{
			"mode":  req.ModeName,
			"error": runErr.Error(),
		})
		return res, fmt.Errorf("retrospective: provider run failed: %w", runErr)
	}

	date := time.Now().UTC().Format("20060102")
	dir := filepath.Join(req.ProjectRoot, ".uta", "context", "retrospectives")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return res, fmt.Errorf("retrospective: mkdir: %w", err)
	}
	path, err := uniqueRetroPath(dir, req.ModeName, date)
	if err != nil {
		return res, fmt.Errorf("retrospective: pick path: %w", err)
	}
	doc := renderRetroDocument(req.ModeName, count, recent, result.FinalText)
	if err := os.WriteFile(path, []byte(doc), 0o644); err != nil {
		return res, fmt.Errorf("retrospective: write %s: %w", path, err)
	}
	res.Path = path

	emitRetro(deps, req.PriorSessionID, trajectory.RetrospectiveCompleted, map[string]any{
		"mode":          req.ModeName,
		"path":          path,
		"session_count": count,
		"summarized":    res.Summarized,
	})
	return res, nil
}

// defaultRetroPromptTmpl is the prompt used when a profile does not set
// retrospective_prompt. The two {{...}} placeholders are filled by
// renderRetroPrompt.
const defaultRetroPromptTmpl = `You are reviewing the last batch of work completed under the "{{mode}}" mission profile in this project.

Recent sessions
---------------
{{sessions}}

Your task
---------
Produce a concise LESSONS_LEARNED markdown for this mode. Cover:

1. What went well — patterns worth repeating.
2. What went wrong — recurring failures, wasted iterations, dead-ends.
3. Open questions — things the next session should investigate.
4. Concrete improvements — guard-rails, prompts, tool changes the operator
   should consider for this mode.

Be specific. Reference concrete sessions / goals from the list above by their
ids when you cite evidence. Keep the document under 500 words.`

// renderRetroPrompt fills the per-mode template (or default) with the mode
// name and a deterministic session-summary block. The block is plain text,
// one bullet per session, with sub-bullets for each session's subtasks
// (title + status + truncated result_text).
func renderRetroPrompt(mode, tmpl string, sessions []store.Session, st *store.Store) string {
	if strings.TrimSpace(tmpl) == "" {
		tmpl = defaultRetroPromptTmpl
	}
	var sb strings.Builder
	for _, s := range sessions {
		fmt.Fprintf(&sb, "- [%s] %s -- status=%s goal=%q\n",
			s.CreatedAt.UTC().Format(time.RFC3339), shortRetroID(s.ID), s.Status, s.Goal)
		if st == nil {
			continue
		}
		subs, err := st.SubtaskListBySession(s.ID, 20, 0)
		if err != nil {
			continue
		}
		for _, sub := range subs {
			summary := strings.TrimSpace(sub.ResultText)
			if sub.Error != "" {
				summary = "error: " + sub.Error
			}
			fmt.Fprintf(&sb, "    * %s [%s] %s\n",
				sub.Title, sub.Status, truncateRetro(summary, 240))
		}
	}
	out := strings.ReplaceAll(tmpl, "{{mode}}", mode)
	out = strings.ReplaceAll(out, "{{sessions}}", strings.TrimRight(sb.String(), "\n"))
	return out
}

// renderRetroDocument wraps the provider's free-form output in a stable
// header so the on-disk file is greppable and self-describing.
func renderRetroDocument(mode string, totalSessions int, sessions []store.Session, body string) string {
	var sb strings.Builder
	now := time.Now().UTC()
	fmt.Fprintf(&sb, "# Retrospective — mode %q\n\n", mode)
	fmt.Fprintf(&sb, "_Generated %s by uta after %d sessions in this mode._\n\n",
		now.Format(time.RFC3339), totalSessions)
	fmt.Fprintf(&sb, "Sessions reviewed (%d, newest first):\n\n", len(sessions))
	for _, s := range sessions {
		fmt.Fprintf(&sb, "- `%s` (%s) — status=%s — %s\n",
			shortRetroID(s.ID), s.CreatedAt.UTC().Format(time.RFC3339),
			s.Status, truncateRetro(s.Goal, 120))
	}
	sb.WriteString("\n---\n\n")
	sb.WriteString(strings.TrimSpace(body))
	sb.WriteString("\n")
	return sb.String()
}

func emitRetro(deps RetroDeps, sessionID string, kind trajectory.Kind, payload map[string]any) {
	if deps.Recorder == nil || sessionID == "" {
		return
	}
	b, err := json.Marshal(payload)
	if err != nil {
		return
	}
	ev := trajectory.Event{
		SessionID: sessionID,
		Seq:       deps.Recorder.AllocSeq(sessionID),
		Ts:        time.Now(),
		Kind:      kind,
		Payload:   json.RawMessage(b),
	}
	deps.Recorder.PersistSync(ev)
	if deps.Bus != nil {
		deps.Bus.Publish(ev)
	}
}

func shortRetroID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

func truncateRetro(s string, n int) string {
	s = strings.TrimSpace(s)
	if n <= 0 || len(s) <= n {
		return s
	}
	if n > 1 {
		return s[:n-1] + "…"
	}
	return s[:n]
}
