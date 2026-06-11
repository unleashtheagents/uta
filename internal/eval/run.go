package eval

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// RunOutcome is what one orchestration run produced — the slice of
// reality the assertions grade. The CLI fills it from engine.RunResult
// + the session row; tests fabricate it directly.
type RunOutcome struct {
	SessionID   string
	Status      string
	FinalAnswer string
	TokensIn    int64
	TokensOut   int64
	USDCents    int64
	Err         error
}

// RunFunc executes one (case, worker) cell and reports the outcome.
// worker may be "" when neither suite nor case pinned one — the
// implementation picks its default.
type RunFunc func(ctx context.Context, c Case, worker string) RunOutcome

// CheckResult is one graded assertion.
type CheckResult struct {
	Name   string `json:"name"`
	Passed bool   `json:"passed"`
	Detail string `json:"detail,omitempty"`
}

// CellResult is one (case, worker) cell of the matrix.
type CellResult struct {
	Case      string        `json:"case"`
	Worker    string        `json:"worker"`
	SessionID string        `json:"session_id,omitempty"`
	Passed    bool          `json:"passed"`
	Elapsed   time.Duration `json:"elapsed_ns"`
	Checks    []CheckResult `json:"checks"`
}

// Report is the full suite outcome.
type Report struct {
	Suite  string       `json:"suite"`
	Cells  []CellResult `json:"cells"`
	Passed int          `json:"passed"`
	Failed int          `json:"failed"`
}

// AllPassed reports whether every cell passed.
func (r *Report) AllPassed() bool { return r.Failed == 0 }

// RunSuite executes every (case × worker) cell sequentially and grades
// each. Sequential on purpose: eval runs measure provider behavior, and
// parallel cells competing for the same provider's rate limits would
// add noise to the very signal the harness exists to capture.
func RunSuite(ctx context.Context, s *Suite, fallbackWorker string, run RunFunc, onProgress func(CellResult)) *Report {
	rep := &Report{Suite: s.Name}
	for i := range s.Cases {
		c := s.Cases[i]
		applyDefaults(&c, s.Defaults)
		for _, worker := range s.matrixFor(&c, fallbackWorker) {
			start := time.Now()
			out := run(ctx, c, worker)
			cell := CellResult{
				Case:      c.Name,
				Worker:    workerLabel(worker),
				SessionID: out.SessionID,
				Elapsed:   time.Since(start),
				Checks:    grade(c, out),
			}
			cell.Passed = true
			for _, ch := range cell.Checks {
				if !ch.Passed {
					cell.Passed = false
					break
				}
			}
			if cell.Passed {
				rep.Passed++
			} else {
				rep.Failed++
			}
			rep.Cells = append(rep.Cells, cell)
			if onProgress != nil {
				onProgress(cell)
			}
			if ctx.Err() != nil {
				return rep
			}
		}
	}
	return rep
}

func applyDefaults(c *Case, d CaseDefaults) {
	if c.Timeout == 0 {
		c.Timeout = d.Timeout
	}
	if c.Mode == "" {
		c.Mode = d.Mode
	}
	if c.Workdir == "" {
		c.Workdir = d.Workdir
	}
}

func workerLabel(w string) string {
	if w == "" {
		return "(default)"
	}
	return w
}

// grade evaluates every configured assertion against the outcome.
// Always returns at least one check (the run-error / status check) so
// an errored run can never accidentally read as "0 checks, passed".
func grade(c Case, out RunOutcome) []CheckResult {
	var checks []CheckResult
	addCheck := func(name string, passed bool, detail string) {
		checks = append(checks, CheckResult{Name: name, Passed: passed, Detail: detail})
	}

	wantStatus := c.Assert.Status
	if wantStatus == "" {
		wantStatus = "completed"
	}
	switch {
	case out.Err != nil:
		addCheck("run", false, "run errored: "+out.Err.Error())
		return checks // nothing else is meaningful on a dead run
	case out.Status != wantStatus:
		addCheck("status", false, fmt.Sprintf("status %q, want %q", out.Status, wantStatus))
	default:
		addCheck("status", true, "")
	}

	answer := out.FinalAnswer
	lower := strings.ToLower(answer)
	for _, want := range c.Assert.Contains {
		ok := strings.Contains(lower, strings.ToLower(want))
		detail := ""
		if !ok {
			detail = fmt.Sprintf("answer missing %q", want)
		}
		addCheck("contains:"+truncate(want, 40), ok, detail)
	}
	for _, deny := range c.Assert.NotContains {
		ok := !strings.Contains(lower, strings.ToLower(deny))
		detail := ""
		if !ok {
			detail = fmt.Sprintf("answer contains forbidden %q", deny)
		}
		addCheck("not_contains:"+truncate(deny, 40), ok, detail)
	}
	if c.Assert.MinChars > 0 {
		ok := len(answer) >= c.Assert.MinChars
		detail := ""
		if !ok {
			detail = fmt.Sprintf("answer is %d chars, want >= %d", len(answer), c.Assert.MinChars)
		}
		addCheck("min_chars", ok, detail)
	}
	if c.Assert.MaxUSDCents > 0 {
		ok := out.USDCents <= c.Assert.MaxUSDCents
		detail := ""
		if !ok {
			detail = fmt.Sprintf("cost %d cents, cap %d", out.USDCents, c.Assert.MaxUSDCents)
		}
		addCheck("max_usd_cents", ok, detail)
	}
	if c.Assert.MaxTokens > 0 {
		total := out.TokensIn + out.TokensOut
		ok := total <= c.Assert.MaxTokens
		detail := ""
		if !ok {
			detail = fmt.Sprintf("%d tokens, cap %d", total, c.Assert.MaxTokens)
		}
		addCheck("max_tokens", ok, detail)
	}
	if c.Assert.Gate != "" {
		ok, detail := runGate(c, answer)
		addCheck("gate", ok, detail)
	}
	return checks
}

// runGate executes the assertion gate command with the final answer on
// stdin and in $UTA_EVAL_ANSWER. 60s cap so a hung gate can't stall the
// suite.
func runGate(c Case, answer string) (bool, string) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "sh", "-c", c.Assert.Gate)
	if c.Workdir != "" {
		cmd.Dir = c.Workdir
	}
	cmd.Stdin = strings.NewReader(answer)
	cmd.Env = append(os.Environ(), "UTA_EVAL_ANSWER="+answer)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return false, fmt.Sprintf("gate failed: %v: %s", err, truncate(strings.TrimSpace(string(out)), 200))
	}
	return true, ""
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
