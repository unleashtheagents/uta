package improve

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/unleashtheagents/uta/internal/provider"
	"github.com/unleashtheagents/uta/internal/store"
	"github.com/unleashtheagents/uta/internal/trajectory"
)

// stubProvider is a minimal AgentProvider so MaybeRetrospective can be
// exercised without a real CLI.
type stubProvider struct {
	name   string
	output string
	runErr error
	called int
	lastPr string
}

func (s *stubProvider) Name() string { return s.name }
func (s *stubProvider) Detect(ctx context.Context) provider.Detection {
	return provider.Detection{Available: true}
}
func (s *stubProvider) RunHeadless(
	ctx context.Context, prompt string,
	opts provider.RunOptions, events chan<- provider.Event,
) (provider.RunResult, error) {
	s.called++
	s.lastPr = prompt
	if s.runErr != nil {
		return provider.RunResult{}, s.runErr
	}
	return provider.RunResult{FinalText: s.output}, nil
}

func openTestStore(t *testing.T) *store.Store {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "t.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func seedSession(t *testing.T, st *store.Store, mode, goal string, ord int) string {
	t.Helper()
	id := "sess-" + mode + "-" + strings.ReplaceAll(time.Now().Format("150405.000000000"), ".", "") + "-" + intStr(ord)
	err := st.CreateSession(store.Session{
		ID:        id,
		Goal:      goal,
		Worker:    "stub",
		Planner:   "stub",
		Status:    "completed",
		CreatedAt: time.Now().Add(time.Duration(ord) * time.Millisecond),
		ModeName:  mode,
	})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	return id
}

func intStr(i int) string {
	if i == 0 {
		return "0"
	}
	var digits []byte
	for i > 0 {
		digits = append([]byte{byte('0' + i%10)}, digits...)
		i /= 10
	}
	return string(digits)
}

func TestMaybeRetrospective_DisabledWhenEveryZero(t *testing.T) {
	st := openTestStore(t)
	reg := provider.NewRegistry()
	res, err := MaybeRetrospective(context.Background(),
		RetroDeps{Store: st, Registry: reg},
		RetroRequest{ModeName: "dev", ProjectRoot: t.TempDir(), WorkerName: "stub", Every: 0},
	)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if res == nil || res.Triggered {
		t.Fatalf("expected non-triggered result, got %+v", res)
	}
}

func TestMaybeRetrospective_DisabledWhenNoProjectRoot(t *testing.T) {
	st := openTestStore(t)
	reg := provider.NewRegistry()
	res, err := MaybeRetrospective(context.Background(),
		RetroDeps{Store: st, Registry: reg},
		RetroRequest{ModeName: "dev", ProjectRoot: "", WorkerName: "stub", Every: 5},
	)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if res.Triggered {
		t.Fatalf("expected non-triggered when no project root, got %+v", res)
	}
}

func TestMaybeRetrospective_OffCadenceNoOp(t *testing.T) {
	st := openTestStore(t)
	reg := provider.NewRegistry()
	prov := &stubProvider{name: "stub", output: "lessons"}
	if err := reg.Register(prov, false); err != nil {
		t.Fatalf("register: %v", err)
	}
	// Seed 4 sessions — Every=5 should not fire yet.
	for i := 0; i < 4; i++ {
		seedSession(t, st, "dev", "g", i)
	}
	res, err := MaybeRetrospective(context.Background(),
		RetroDeps{Store: st, Registry: reg},
		RetroRequest{ModeName: "dev", ProjectRoot: t.TempDir(), WorkerName: "stub", Every: 5},
	)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if res.Triggered {
		t.Fatalf("expected off-cadence skip at 4 sessions, got %+v", res)
	}
	if res.SessionCount != 4 {
		t.Errorf("count = %d want 4", res.SessionCount)
	}
	if prov.called != 0 {
		t.Errorf("provider should not have been called, was %d", prov.called)
	}
}

func TestMaybeRetrospective_FiresOnCadence_WritesMarkdown(t *testing.T) {
	st := openTestStore(t)
	reg := provider.NewRegistry()
	prov := &stubProvider{
		name:   "stub",
		output: "## Lessons\n\nUse smaller diffs.\n",
	}
	if err := reg.Register(prov, false); err != nil {
		t.Fatalf("register: %v", err)
	}
	// Seed exactly 5 sessions so Every=5 fires.
	for i := 0; i < 5; i++ {
		seedSession(t, st, "dev", "goal-"+intStr(i), i)
	}
	projectRoot := t.TempDir()
	res, err := MaybeRetrospective(context.Background(),
		RetroDeps{Store: st, Registry: reg},
		RetroRequest{
			ModeName:    "dev",
			ProjectRoot: projectRoot,
			WorkerName:  "stub",
			Every:       5,
		},
	)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !res.Triggered {
		t.Fatalf("expected Triggered=true on cadence, got %+v", res)
	}
	if res.Path == "" {
		t.Fatal("expected non-empty Path")
	}
	if !strings.Contains(res.Path, filepath.Join(".uta", "context", "retrospectives")) {
		t.Errorf("path not under retrospectives dir: %s", res.Path)
	}
	if prov.called != 1 {
		t.Errorf("provider call count = %d want 1", prov.called)
	}
	if !strings.Contains(prov.lastPr, "dev") {
		t.Errorf("prompt missing mode name, got: %s", prov.lastPr)
	}
	if !strings.Contains(prov.lastPr, "goal-0") {
		t.Errorf("prompt missing seeded goal, got: %s", prov.lastPr)
	}

	data, rerr := os.ReadFile(res.Path)
	if rerr != nil {
		t.Fatalf("read written file: %v", rerr)
	}
	body := string(data)
	if !strings.Contains(body, "Retrospective") {
		t.Errorf("file missing header, body=%q", body)
	}
	if !strings.Contains(body, "Use smaller diffs.") {
		t.Errorf("file missing provider output, body=%q", body)
	}
}

func TestMaybeRetrospective_OnlyCountsTargetMode(t *testing.T) {
	st := openTestStore(t)
	reg := provider.NewRegistry()
	prov := &stubProvider{name: "stub", output: "x"}
	if err := reg.Register(prov, false); err != nil {
		t.Fatalf("register: %v", err)
	}
	// 5 sessions in "audit", only 2 in "dev". Every=5 for "dev" should
	// NOT fire (other modes don't contribute).
	for i := 0; i < 5; i++ {
		seedSession(t, st, "audit", "a", i)
	}
	for i := 0; i < 2; i++ {
		seedSession(t, st, "dev", "d", i)
	}
	res, err := MaybeRetrospective(context.Background(),
		RetroDeps{Store: st, Registry: reg},
		RetroRequest{ModeName: "dev", ProjectRoot: t.TempDir(), WorkerName: "stub", Every: 5},
	)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if res.Triggered {
		t.Fatalf("dev cadence fired off audit sessions: %+v", res)
	}
	if res.SessionCount != 2 {
		t.Errorf("dev count = %d want 2", res.SessionCount)
	}
}

func TestMaybeRetrospective_EmitsTrajectoryEvents(t *testing.T) {
	st := openTestStore(t)
	reg := provider.NewRegistry()
	prov := &stubProvider{name: "stub", output: "ok"}
	if err := reg.Register(prov, false); err != nil {
		t.Fatalf("register: %v", err)
	}
	priorSession := seedSession(t, st, "dev", "prior", 0)
	for i := 1; i < 5; i++ {
		seedSession(t, st, "dev", "g", i)
	}
	recorder := trajectory.NewRecorder(st)
	res, err := MaybeRetrospective(context.Background(),
		RetroDeps{Store: st, Registry: reg, Recorder: recorder},
		RetroRequest{
			ModeName: "dev", ProjectRoot: t.TempDir(),
			WorkerName: "stub", Every: 5,
			PriorSessionID: priorSession,
		},
	)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !res.Triggered {
		t.Fatal("expected trigger")
	}
	events, err := st.ListEvents(priorSession, 0, 0)
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	var started, completed bool
	for _, e := range events {
		if e.Kind == string(trajectory.RetrospectiveStarted) {
			started = true
		}
		if e.Kind == string(trajectory.RetrospectiveCompleted) {
			completed = true
		}
	}
	if !started || !completed {
		t.Fatalf("missing trajectory events: started=%v completed=%v (got %d events)",
			started, completed, len(events))
	}
}

func TestRenderRetroPrompt_UsesCustomTemplate(t *testing.T) {
	sessions := []store.Session{
		{ID: "abcdef1234", Goal: "fix bug", Status: "completed", CreatedAt: time.Unix(1_700_000_000, 0)},
	}
	got := renderRetroPrompt("dev", "MODE={{mode}} SESS={{sessions}}", sessions, nil)
	if !strings.Contains(got, "MODE=dev") {
		t.Errorf("missing {{mode}} substitution: %s", got)
	}
	if !strings.Contains(got, "SESS=") {
		t.Errorf("missing {{sessions}} substitution: %s", got)
	}
	if !strings.Contains(got, "fix bug") {
		t.Errorf("missing seeded goal: %s", got)
	}
}

func TestRenderRetroPrompt_FallsBackToDefault(t *testing.T) {
	sessions := []store.Session{
		{ID: "x", Goal: "g", Status: "completed", CreatedAt: time.Unix(1_700_000_000, 0)},
	}
	got := renderRetroPrompt("dev", "", sessions, nil)
	if !strings.Contains(got, "LESSONS_LEARNED") {
		t.Errorf("default prompt missing LESSONS_LEARNED hint: %s", got)
	}
}

func TestRenderRetroPrompt_EmptySessions(t *testing.T) {
	// Direct invocation with nil/empty sessions must still substitute both
	// placeholders cleanly — caller may rely on prompt being well-formed
	// even when nothing was found.
	got := renderRetroPrompt("dev", "MODE={{mode}} SESS=[{{sessions}}]", nil, nil)
	if !strings.Contains(got, "MODE=dev") {
		t.Errorf("missing mode substitution on empty input: %s", got)
	}
	if !strings.Contains(got, "SESS=[]") {
		t.Errorf("expected empty sessions to substitute as empty string: %s", got)
	}
}

func TestRenderRetroPrompt_VeryLongGoalPreserved(t *testing.T) {
	long := strings.Repeat("x", 5000)
	sessions := []store.Session{
		{ID: "abcdef1234", Goal: long, Status: "completed", CreatedAt: time.Unix(1_700_000_000, 0)},
	}
	got := renderRetroPrompt("dev", "{{sessions}}", sessions, nil)
	// Goal in prompt is intentionally not truncated — only subtask result_text
	// is. Verify the long goal survives substitution.
	if !strings.Contains(got, long) {
		t.Errorf("very long goal did not survive prompt substitution")
	}
}

func TestListRetrospectives_FindsAndParses(t *testing.T) {
	projectRoot := t.TempDir()
	dir := RetrospectiveDir(projectRoot)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	for _, name := range []string{"dev-20260101.md", "audit-20251231.md", "not-a-retro.txt"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	retros, err := ListRetrospectives(projectRoot)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(retros) != 2 {
		t.Fatalf("got %d files, want 2 (non-md must be filtered)", len(retros))
	}
	gotModes := map[string]bool{}
	for _, r := range retros {
		gotModes[r.Mode] = true
	}
	if !gotModes["dev"] || !gotModes["audit"] {
		t.Errorf("modes = %v, want dev+audit", gotModes)
	}
}

func TestListRetrospectives_MissingDirIsNotAnError(t *testing.T) {
	retros, err := ListRetrospectives(t.TempDir())
	if err != nil {
		t.Fatalf("err on missing dir: %v", err)
	}
	if len(retros) != 0 {
		t.Errorf("expected empty list, got %d", len(retros))
	}
}

func TestListRetrospectives_EmptyProjectRootReturnsNil(t *testing.T) {
	retros, err := ListRetrospectives("")
	if err != nil || retros != nil {
		t.Errorf("empty root: got %v / %v, want nil / nil", retros, err)
	}
}

func TestParseRetroMode(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"dev-20260101.md", "dev"},
		{"some-mode-20260101.md", "some-mode"},
		{"dev-20260101-2.md", "dev"},
		{"some-mode-20260101-12.md", "some-mode"},
		{"no-date.md", ""},
		{"weird-2026Q1.md", ""},
		{"-20260101.md", ""},
		{"plain.md", ""},
		{"dev-20260101-.md", ""},  // empty counter
		{"dev-2026010-2.md", ""},  // bad date length
		{"dev-2026010A-2.md", ""}, // non-digit date
		{"no-extension", ""},
	}
	for _, c := range cases {
		if got := parseRetroMode(c.in); got != c.want {
			t.Errorf("parseRetroMode(%q)=%q want %q", c.in, got, c.want)
		}
	}
}

func TestUniqueRetroPath_AppendsCounterOnCollision(t *testing.T) {
	dir := t.TempDir()
	p1, err := uniqueRetroPath(dir, "dev", "20260101")
	if err != nil {
		t.Fatalf("first call: %v", err)
	}
	if filepath.Base(p1) != "dev-20260101.md" {
		t.Errorf("first path = %s, want dev-20260101.md", filepath.Base(p1))
	}
	if err := os.WriteFile(p1, []byte("x"), 0o644); err != nil {
		t.Fatalf("write first: %v", err)
	}
	p2, err := uniqueRetroPath(dir, "dev", "20260101")
	if err != nil {
		t.Fatalf("second call: %v", err)
	}
	if filepath.Base(p2) != "dev-20260101-2.md" {
		t.Errorf("second path = %s, want dev-20260101-2.md", filepath.Base(p2))
	}
	if err := os.WriteFile(p2, []byte("y"), 0o644); err != nil {
		t.Fatalf("write second: %v", err)
	}
	p3, err := uniqueRetroPath(dir, "dev", "20260101")
	if err != nil {
		t.Fatalf("third call: %v", err)
	}
	if filepath.Base(p3) != "dev-20260101-3.md" {
		t.Errorf("third path = %s, want dev-20260101-3.md", filepath.Base(p3))
	}
}

func TestMaybeRetrospective_SameDaySecondRunDoesNotOverwrite(t *testing.T) {
	st := openTestStore(t)
	reg := provider.NewRegistry()
	prov := &stubProvider{name: "stub", output: "first body"}
	if err := reg.Register(prov, false); err != nil {
		t.Fatalf("register: %v", err)
	}
	for i := 0; i < 5; i++ {
		seedSession(t, st, "dev", "goal-"+intStr(i), i)
	}
	projectRoot := t.TempDir()
	res1, err := MaybeRetrospective(context.Background(),
		RetroDeps{Store: st, Registry: reg},
		RetroRequest{ModeName: "dev", ProjectRoot: projectRoot, WorkerName: "stub", Every: 5},
	)
	if err != nil || !res1.Triggered {
		t.Fatalf("first retro must trigger: %+v err=%v", res1, err)
	}
	// Five more sessions to hit the next cadence boundary same UTC day.
	for i := 5; i < 10; i++ {
		seedSession(t, st, "dev", "goal-"+intStr(i), i)
	}
	prov.output = "second body"
	res2, err := MaybeRetrospective(context.Background(),
		RetroDeps{Store: st, Registry: reg},
		RetroRequest{ModeName: "dev", ProjectRoot: projectRoot, WorkerName: "stub", Every: 5},
	)
	if err != nil || !res2.Triggered {
		t.Fatalf("second retro must trigger: %+v err=%v", res2, err)
	}
	if res1.Path == res2.Path {
		t.Fatalf("second retro overwrote first; both wrote to %s", res1.Path)
	}
	b1, err := os.ReadFile(res1.Path)
	if err != nil {
		t.Fatalf("read first: %v", err)
	}
	if !strings.Contains(string(b1), "first body") {
		t.Errorf("first file lost its body, got: %s", b1)
	}
	b2, err := os.ReadFile(res2.Path)
	if err != nil {
		t.Fatalf("read second: %v", err)
	}
	if !strings.Contains(string(b2), "second body") {
		t.Errorf("second file missing its body, got: %s", b2)
	}
}

func TestMaybeRetrospective_UnknownWorkerReturnsError(t *testing.T) {
	// When the configured worker is not in the registry the cadence boundary
	// has been hit but synthesis cannot proceed. The caller must see a
	// surfaced error, no file written, and Triggered must remain false.
	st := openTestStore(t)
	reg := provider.NewRegistry()
	for i := 0; i < 5; i++ {
		seedSession(t, st, "dev", "g", i)
	}
	projectRoot := t.TempDir()
	res, err := MaybeRetrospective(context.Background(),
		RetroDeps{Store: st, Registry: reg},
		RetroRequest{
			ModeName: "dev", ProjectRoot: projectRoot,
			WorkerName: "missing", Every: 5,
		},
	)
	if err == nil {
		t.Fatal("expected error for unregistered worker, got nil")
	}
	if !strings.Contains(err.Error(), "missing") {
		t.Errorf("error should name the missing worker, got: %v", err)
	}
	if res != nil && res.Triggered {
		t.Errorf("Triggered must stay false on unknown worker, got %+v", res)
	}
	if res != nil && res.Path != "" {
		t.Errorf("no file should be written on unknown worker, got path=%q", res.Path)
	}
	// No file may be created under the project root either.
	if entries, _ := os.ReadDir(filepath.Join(projectRoot, ".uta", "context", "retrospectives")); len(entries) != 0 {
		t.Errorf("retrospective dir should be empty on unknown worker, got %d entries", len(entries))
	}
}

func TestMaybeRetrospective_ProviderFailureWrapsErrorAndEmitsCompleted(t *testing.T) {
	// Provider returning an error must (a) surface a wrapped error to the
	// caller and (b) still emit a retrospective_completed trajectory event
	// carrying the error payload, so observers can tell the synthesis was
	// attempted but failed rather than skipped.
	st := openTestStore(t)
	reg := provider.NewRegistry()
	prov := &stubProvider{name: "stub", runErr: errBoom}
	if err := reg.Register(prov, false); err != nil {
		t.Fatalf("register: %v", err)
	}
	priorSession := seedSession(t, st, "dev", "prior", 0)
	for i := 1; i < 5; i++ {
		seedSession(t, st, "dev", "g", i)
	}
	recorder := trajectory.NewRecorder(st)
	res, err := MaybeRetrospective(context.Background(),
		RetroDeps{Store: st, Registry: reg, Recorder: recorder},
		RetroRequest{
			ModeName: "dev", ProjectRoot: t.TempDir(),
			WorkerName: "stub", Every: 5,
			PriorSessionID: priorSession,
		},
	)
	if err == nil {
		t.Fatal("expected wrapped provider error, got nil")
	}
	if !strings.Contains(err.Error(), "boom") {
		t.Errorf("error must wrap provider failure, got: %v", err)
	}
	if res == nil || !res.Triggered {
		t.Errorf("Triggered should be true once cadence fires, got %+v", res)
	}
	if res != nil && res.Path != "" {
		t.Errorf("no file must be written on provider failure, got path=%q", res.Path)
	}
	events, lerr := st.ListEvents(priorSession, 0, 0)
	if lerr != nil {
		t.Fatalf("list events: %v", lerr)
	}
	var sawCompletedWithError bool
	for _, e := range events {
		if e.Kind != string(trajectory.RetrospectiveCompleted) {
			continue
		}
		if strings.Contains(string(e.Payload), "boom") {
			sawCompletedWithError = true
		}
	}
	if !sawCompletedWithError {
		t.Fatalf("expected retrospective_completed event carrying provider error; got events=%+v", events)
	}
}

var errBoom = errBoomT("boom")

type errBoomT string

func (e errBoomT) Error() string { return string(e) }
