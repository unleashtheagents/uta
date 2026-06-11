package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/unleashtheagents/uta/internal/config"
	"github.com/unleashtheagents/uta/internal/improve"
	"github.com/unleashtheagents/uta/internal/paths"
	"github.com/unleashtheagents/uta/internal/store"
)

// setupIdeasProject creates an isolated HOME, an isolated UTA global home,
// and an initialized project rooted at a fresh temp dir. It chdir's into the
// project for the duration of the test and restores the prior cwd on cleanup.
// Returns the absolute project root so tests can poke at the project DB
// directly if they need to seed or assert outside the CLI.
func setupIdeasProject(t *testing.T) string {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("UTA_HOME", "")

	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}
	if err := config.SaveProject(root, &config.Project{Name: "ideas-test"}); err != nil {
		t.Fatalf("SaveProject: %v", err)
	}

	prev, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	if err := os.Chdir(root); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(prev) })
	return root
}

// runIdeas builds a fresh ideas command tree and executes it with the given
// args and stdin. Returns stdout, stderr, and the executor error.
func runIdeas(t *testing.T, stdin string, args ...string) (string, string, error) {
	t.Helper()
	cmd := newIdeasCmd()
	var out, errBuf bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errBuf)
	cmd.SetIn(strings.NewReader(stdin))
	cmd.SetArgs(args)
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	err := cmd.ExecuteContext(context.Background())
	return out.String(), errBuf.String(), err
}

// openIdeasBoard opens the project DB directly so tests can seed and inspect
// idea rows without going through the CLI. The caller is responsible for
// closing the store via the returned cleanup.
func openIdeasBoard(t *testing.T, root string) (*improve.Board, func()) {
	t.Helper()
	st, err := store.Open(paths.DB(paths.ProjectStateDir(root)))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	return improve.NewBoard(st), func() { st.Close() }
}

func TestIdeasAdd_PersistsIdea(t *testing.T) {
	root := setupIdeasProject(t)

	stdout, _, err := runIdeas(t, "",
		"add",
		"--title", "Refactor X",
		"--body", "Split god struct into smaller pieces",
		"--severity", "high",
		"--tag", "refactor",
		"--tag", "tech-debt",
	)
	if err != nil {
		t.Fatalf("add: %v", err)
	}
	if !strings.Contains(stdout, "Refactor X") {
		t.Errorf("add stdout missing title: %q", stdout)
	}

	board, cleanup := openIdeasBoard(t, root)
	defer cleanup()
	ideas, err := board.List(improve.ListOptions{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(ideas) != 1 {
		t.Fatalf("expected 1 idea persisted, got %d", len(ideas))
	}
	got := ideas[0]
	if got.Title != "Refactor X" {
		t.Errorf("title = %q, want %q", got.Title, "Refactor X")
	}
	if got.Body != "Split god struct into smaller pieces" {
		t.Errorf("body = %q", got.Body)
	}
	if got.Severity != improve.SevHigh {
		t.Errorf("severity = %q, want %q", got.Severity, improve.SevHigh)
	}
	if got.Status != improve.StatusProposed {
		t.Errorf("status = %q, want %q (default)", got.Status, improve.StatusProposed)
	}
	if got.Source != "manual" {
		t.Errorf("source = %q, want %q", got.Source, "manual")
	}
	if len(got.Tags) != 2 || got.Tags[0] != "refactor" || got.Tags[1] != "tech-debt" {
		t.Errorf("tags = %v, want [refactor tech-debt]", got.Tags)
	}
}

func TestIdeasAdd_RequiresTitle(t *testing.T) {
	setupIdeasProject(t)

	_, _, err := runIdeas(t, "", "add", "--body", "no title here")
	if err == nil {
		t.Fatalf("add without --title: want error, got nil")
	}
	if !strings.Contains(err.Error(), "title") {
		t.Errorf("error should mention title: %v", err)
	}
}

func TestIdeasAdd_BodyFromStdin(t *testing.T) {
	root := setupIdeasProject(t)

	payload := "this body\ncame from\nstdin"
	if _, _, err := runIdeas(t, payload,
		"add",
		"--title", "Stdin idea",
		"--body-stdin",
	); err != nil {
		t.Fatalf("add --body-stdin: %v", err)
	}

	board, cleanup := openIdeasBoard(t, root)
	defer cleanup()
	ideas, err := board.List(improve.ListOptions{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(ideas) != 1 {
		t.Fatalf("expected 1 idea persisted, got %d", len(ideas))
	}
	if ideas[0].Body != payload {
		t.Errorf("body = %q, want %q", ideas[0].Body, payload)
	}
}

func TestIdeasList_TextEmpty(t *testing.T) {
	setupIdeasProject(t)

	out, _, err := runIdeas(t, "", "list")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if !strings.Contains(out, "0 idea(s)") {
		t.Errorf("empty list should report 0 ideas: %q", out)
	}
	// Header row should still render even on an empty board.
	for _, h := range []string{"ID", "SEV", "STATUS", "SOURCE", "TITLE"} {
		if !strings.Contains(out, h) {
			t.Errorf("empty list missing header %q:\n%s", h, out)
		}
	}
}

func TestIdeasList_TextShowsRowsAndStats(t *testing.T) {
	root := setupIdeasProject(t)

	board, cleanup := openIdeasBoard(t, root)
	if err := board.Insert(&improve.Idea{Title: "Alpha bug", Severity: improve.SevHigh}); err != nil {
		t.Fatalf("seed alpha: %v", err)
	}
	if err := board.Insert(&improve.Idea{Title: "Beta polish", Severity: improve.SevLow}); err != nil {
		t.Fatalf("seed beta: %v", err)
	}
	cleanup() // close before the CLI re-opens the DB

	out, _, err := runIdeas(t, "", "list")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if !strings.Contains(out, "2 idea(s)") {
		t.Errorf("list should report 2 ideas: %q", out)
	}
	if !strings.Contains(out, "proposed=2") {
		t.Errorf("stats should show proposed=2: %q", out)
	}
	for _, want := range []string{"Alpha bug", "Beta polish"} {
		if !strings.Contains(out, want) {
			t.Errorf("list output missing %q:\n%s", want, out)
		}
	}
}

func TestIdeasList_FiltersByStatus(t *testing.T) {
	root := setupIdeasProject(t)

	board, cleanup := openIdeasBoard(t, root)
	accepted := &improve.Idea{Title: "Accepted one", Status: improve.StatusAccepted}
	proposed := &improve.Idea{Title: "Proposed one", Status: improve.StatusProposed}
	if err := board.Insert(accepted); err != nil {
		t.Fatalf("seed accepted: %v", err)
	}
	if err := board.Insert(proposed); err != nil {
		t.Fatalf("seed proposed: %v", err)
	}
	cleanup()

	out, _, err := runIdeas(t, "", "list", "--status", "accepted", "--json")
	if err != nil {
		t.Fatalf("list --status: %v", err)
	}
	var ideas []*improve.Idea
	if err := json.Unmarshal([]byte(out), &ideas); err != nil {
		t.Fatalf("decode json: %v\nraw=%s", err, out)
	}
	if len(ideas) != 1 {
		t.Fatalf("expected 1 idea filtered, got %d: %+v", len(ideas), ideas)
	}
	if ideas[0].Title != "Accepted one" {
		t.Errorf("filter mismatch: %+v", ideas[0])
	}
}

func TestIdeasShow_TextOutput(t *testing.T) {
	root := setupIdeasProject(t)

	board, cleanup := openIdeasBoard(t, root)
	seeded := &improve.Idea{
		Title:    "Show me",
		Body:     "body lines here",
		Severity: improve.SevMedium,
		Source:   "manual",
		Tags:     []string{"alpha", "beta"},
	}
	if err := board.Insert(seeded); err != nil {
		t.Fatalf("seed: %v", err)
	}
	cleanup()

	out, _, err := runIdeas(t, "", "show", seeded.ID)
	if err != nil {
		t.Fatalf("show: %v", err)
	}
	for _, want := range []string{
		"id:        " + seeded.ID,
		"title:     Show me",
		"severity:  medium",
		"status:    proposed",
		"source:    manual",
		"tags:      alpha, beta",
		"body:",
		"body lines here",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("show output missing %q:\n%s", want, out)
		}
	}
}

func TestIdeasShow_JSON(t *testing.T) {
	root := setupIdeasProject(t)

	board, cleanup := openIdeasBoard(t, root)
	seeded := &improve.Idea{Title: "JSON me", Severity: improve.SevLow}
	if err := board.Insert(seeded); err != nil {
		t.Fatalf("seed: %v", err)
	}
	cleanup()

	out, _, err := runIdeas(t, "", "show", seeded.ID, "--json")
	if err != nil {
		t.Fatalf("show --json: %v", err)
	}
	var got improve.Idea
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("decode json: %v\nraw=%s", err, out)
	}
	if got.ID != seeded.ID {
		t.Errorf("id mismatch: got %q want %q", got.ID, seeded.ID)
	}
	if got.Title != "JSON me" {
		t.Errorf("title mismatch: %q", got.Title)
	}
}

func TestIdeasShow_MissingErrors(t *testing.T) {
	setupIdeasProject(t)

	_, _, err := runIdeas(t, "", "show", "00000000-0000-0000-0000-000000000000")
	if err == nil {
		t.Fatalf("show missing: want error, got nil")
	}
}
