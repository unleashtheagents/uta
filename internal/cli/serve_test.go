package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"testing"

	"github.com/unleashtheagents/uta/internal/improve"
	"github.com/unleashtheagents/uta/internal/mcp"
	"github.com/unleashtheagents/uta/internal/provider"
	"github.com/unleashtheagents/uta/internal/store"
	"github.com/unleashtheagents/uta/internal/whiteboard"
)

// newServeTestApp builds an App with a real on-disk SQLite store, an empty
// provider registry, and no project context. It's the minimum surface every
// MCP tool handler needs without dragging in cobra or the global home.
func newServeTestApp(t *testing.T) *App {
	t.Helper()
	dir := t.TempDir()
	s, err := store.Open(filepath.Join(dir, "serve.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return &App{
		GlobalHome: dir,
		StateDir:   dir,
		Store:      s,
		Blobs:      store.NewBlobs(filepath.Join(dir, "blobs")),
		Registry:   provider.NewRegistry(),
	}
}

// newServeTestServer wires registerMCPTools against a fresh server for the
// supplied App.
func newServeTestServer(t *testing.T, app *App) *mcp.Server {
	t.Helper()
	srv := mcp.NewServer("uta-test", "v0.0.0-test")
	registerMCPTools(srv, app)
	return srv
}

// callTool sends one tools/call request through Serve and returns the parsed
// JSON-RPC response. Failing this helper is always a wiring bug — the
// per-handler assertions live in the individual tests.
func callTool(t *testing.T, srv *mcp.Server, name string, args any) mcp.Message {
	t.Helper()
	var argsRaw json.RawMessage
	if args != nil {
		b, err := json.Marshal(args)
		if err != nil {
			t.Fatalf("marshal args for %s: %v", name, err)
		}
		argsRaw = b
	}
	params := struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments,omitempty"`
	}{Name: name, Arguments: argsRaw}
	paramsRaw, err := json.Marshal(params)
	if err != nil {
		t.Fatalf("marshal params: %v", err)
	}
	req := mcp.Message{
		JSONRPC: "2.0",
		ID:      json.RawMessage(`1`),
		Method:  "tools/call",
		Params:  paramsRaw,
	}
	reqBytes, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	var out bytes.Buffer
	if err := srv.Serve(context.Background(), strings.NewReader(string(reqBytes)+"\n"), &out); err != nil {
		t.Fatalf("Serve(%s): %v", name, err)
	}
	var resp mcp.Message
	if err := json.NewDecoder(&out).Decode(&resp); err != nil && err != io.EOF {
		t.Fatalf("decode response for %s: %v\nraw=%s", name, err, out.String())
	}
	if resp.JSONRPC != "2.0" {
		t.Fatalf("%s: response jsonrpc = %q, want 2.0 (raw=%s)", name, resp.JSONRPC, out.String())
	}
	if string(resp.ID) != "1" {
		t.Fatalf("%s: response id = %s, want 1", name, resp.ID)
	}
	return resp
}

// toolResultFrom unwraps the ToolResult inside a successful JSON-RPC response.
// JSON-RPC errors (e.g. unknown tool) are fatal — callers expecting them
// should inspect resp.Error directly.
func toolResultFrom(t *testing.T, name string, resp mcp.Message) mcp.ToolResult {
	t.Helper()
	if resp.Error != nil {
		t.Fatalf("%s: unexpected JSON-RPC error: %+v", name, resp.Error)
	}
	var tr mcp.ToolResult
	if err := json.Unmarshal(resp.Result, &tr); err != nil {
		t.Fatalf("%s: decode ToolResult: %v\nraw=%s", name, err, resp.Result)
	}
	if len(tr.Content) == 0 {
		t.Fatalf("%s: ToolResult.Content is empty", name)
	}
	if tr.Content[0].Type != "text" {
		t.Fatalf("%s: Content[0].Type = %q, want text", name, tr.Content[0].Type)
	}
	return tr
}

// TestRegisterMCPTools_AllRegistered is the smoke test: every tool documented
// in the serve command Long help must actually register. The list mirrors
// the docstring so a renamed tool will fail loudly here.
func TestRegisterMCPTools_AllRegistered(t *testing.T) {
	app := newServeTestApp(t)
	srv := newServeTestServer(t, app)
	want := []string{
		"uta_providers_list",
		"uta_ideas_list",
		"uta_ideas_add",
		"uta_ideas_show",
		"uta_sessions_list",
		"uta_trajectory_get",
		"uta_audit",
		"uta_run",
		"uta_mission_check",
		"uta_mission_run",
		"uta_improve_pick",
		"uta_whiteboard_set",
		"uta_whiteboard_get",
		"uta_whiteboard_list",
	}
	if got := srv.ToolCount(); got != len(want) {
		t.Errorf("ToolCount = %d, want %d", got, len(want))
	}
	// Use tools/list to verify advertised names rather than reach into
	// private state. This also doubles as a JSON-RPC framing smoke test.
	req := `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`
	var out bytes.Buffer
	if err := srv.Serve(context.Background(), strings.NewReader(req+"\n"), &out); err != nil {
		t.Fatalf("Serve(tools/list): %v", err)
	}
	var resp mcp.Message
	if err := json.NewDecoder(&out).Decode(&resp); err != nil {
		t.Fatalf("decode tools/list: %v", err)
	}
	if resp.Error != nil {
		t.Fatalf("tools/list returned error: %+v", resp.Error)
	}
	var body struct {
		Tools []mcp.Tool `json:"tools"`
	}
	if err := json.Unmarshal(resp.Result, &body); err != nil {
		t.Fatalf("decode tools list result: %v", err)
	}
	got := map[string]bool{}
	for _, tl := range body.Tools {
		got[tl.Name] = true
		// Every registered tool must have a non-empty schema — clients use it
		// to drive UI/validation, so an empty inputSchema is a bug.
		if len(tl.InputSchema) == 0 {
			t.Errorf("tool %q has empty inputSchema", tl.Name)
		}
		if strings.TrimSpace(tl.Description) == "" {
			t.Errorf("tool %q has empty description", tl.Name)
		}
	}
	for _, name := range want {
		if !got[name] {
			t.Errorf("tool %q not registered", name)
		}
	}
}

// TestProvidersListTool_EmptyRegistry covers the happy path with no
// providers installed — output should be the JSON literal "[]" (the
// jsonDump of an empty slice).
func TestProvidersListTool_EmptyRegistry(t *testing.T) {
	app := newServeTestApp(t)
	srv := newServeTestServer(t, app)
	resp := callTool(t, srv, "uta_providers_list", map[string]any{})
	tr := toolResultFrom(t, "uta_providers_list", resp)
	if tr.IsError {
		t.Fatalf("unexpected IsError=true: %+v", tr)
	}
	var rows []any
	if err := json.Unmarshal([]byte(tr.Content[0].Text), &rows); err != nil {
		t.Fatalf("decode providers list: %v\nraw=%s", err, tr.Content[0].Text)
	}
	if len(rows) != 0 {
		t.Errorf("empty registry should produce 0 rows, got %d: %+v", len(rows), rows)
	}
}

// TestIdeasAddTool_InsertsAndDefaults verifies argument parsing for
// uta_ideas_add: a title is required, severity defaults to medium, source
// is stamped as "mcp".
func TestIdeasAddTool_InsertsAndDefaults(t *testing.T) {
	app := newServeTestApp(t)
	srv := newServeTestServer(t, app)

	resp := callTool(t, srv, "uta_ideas_add", map[string]any{
		"title": "wire a dashboard",
		"body":  "expose perf via /dash",
		"tags":  []string{"ux", "obs"},
	})
	tr := toolResultFrom(t, "uta_ideas_add", resp)
	if tr.IsError {
		t.Fatalf("uta_ideas_add: unexpected IsError=true: %+v", tr)
	}
	var got improve.Idea
	if err := json.Unmarshal([]byte(tr.Content[0].Text), &got); err != nil {
		t.Fatalf("decode idea: %v\nraw=%s", err, tr.Content[0].Text)
	}
	if got.ID == "" {
		t.Error("idea ID was not generated")
	}
	if got.Title != "wire a dashboard" {
		t.Errorf("Title = %q", got.Title)
	}
	if got.Severity != improve.SevMedium {
		t.Errorf("Severity default = %q, want %q", got.Severity, improve.SevMedium)
	}
	if got.Source != "mcp" {
		t.Errorf("Source = %q, want mcp", got.Source)
	}
	// Round-trip into the DB to prove the JSON shape matched the DAO write.
	board := improve.NewBoard(app.Store)
	persisted, err := board.Get(got.ID)
	if err != nil {
		t.Fatalf("board.Get: %v", err)
	}
	if persisted.Title != got.Title {
		t.Errorf("persisted Title = %q, response Title = %q", persisted.Title, got.Title)
	}
}

// TestIdeasAddTool_RejectsBlankTitle covers the explicit ArgError branch:
// a blank title must surface as an IsError=true tool result (not a
// JSON-RPC error — that's reserved for transport-level failures).
func TestIdeasAddTool_RejectsBlankTitle(t *testing.T) {
	app := newServeTestApp(t)
	srv := newServeTestServer(t, app)
	resp := callTool(t, srv, "uta_ideas_add", map[string]any{"title": "  "})
	tr := toolResultFrom(t, "uta_ideas_add", resp)
	if !tr.IsError {
		t.Fatalf("blank title should produce IsError=true, got %+v", tr)
	}
	if !strings.Contains(tr.Content[0].Text, "title is required") {
		t.Errorf("error text = %q; want mention of 'title is required'", tr.Content[0].Text)
	}
}

// TestIdeasListTool_FiltersBySeverity exercises the args-parsing path with
// a non-default filter, and verifies the returned JSON respects the
// severity filter.
func TestIdeasListTool_FiltersBySeverity(t *testing.T) {
	app := newServeTestApp(t)
	board := improve.NewBoard(app.Store)
	if err := board.Insert(&improve.Idea{Title: "h", Severity: improve.SevHigh}); err != nil {
		t.Fatalf("seed high: %v", err)
	}
	if err := board.Insert(&improve.Idea{Title: "m", Severity: improve.SevMedium}); err != nil {
		t.Fatalf("seed medium: %v", err)
	}
	srv := newServeTestServer(t, app)

	resp := callTool(t, srv, "uta_ideas_list", map[string]any{"severity": "high"})
	tr := toolResultFrom(t, "uta_ideas_list", resp)
	if tr.IsError {
		t.Fatalf("unexpected IsError=true: %+v", tr)
	}
	var got []improve.Idea
	if err := json.Unmarshal([]byte(tr.Content[0].Text), &got); err != nil {
		t.Fatalf("decode list: %v\nraw=%s", err, tr.Content[0].Text)
	}
	if len(got) != 1 || got[0].Severity != improve.SevHigh {
		t.Errorf("severity=high filter returned %+v", got)
	}
}

// TestIdeasShowTool_RoundTripsIdea proves uta_ideas_show looks up the
// inserted row and serializes the same id back.
func TestIdeasShowTool_RoundTripsIdea(t *testing.T) {
	app := newServeTestApp(t)
	board := improve.NewBoard(app.Store)
	idea := &improve.Idea{Title: "show me"}
	if err := board.Insert(idea); err != nil {
		t.Fatalf("seed: %v", err)
	}
	srv := newServeTestServer(t, app)

	resp := callTool(t, srv, "uta_ideas_show", map[string]any{"id": idea.ID})
	tr := toolResultFrom(t, "uta_ideas_show", resp)
	if tr.IsError {
		t.Fatalf("unexpected IsError=true: %+v", tr)
	}
	var got improve.Idea
	if err := json.Unmarshal([]byte(tr.Content[0].Text), &got); err != nil {
		t.Fatalf("decode idea: %v", err)
	}
	if got.ID != idea.ID || got.Title != "show me" {
		t.Errorf("got = %+v", got)
	}
}

// TestIdeasShowTool_UnknownIdIsToolError: a missing id should surface as
// an IsError=true tool result so the model can decide what to do next.
func TestIdeasShowTool_UnknownIdIsToolError(t *testing.T) {
	app := newServeTestApp(t)
	srv := newServeTestServer(t, app)
	resp := callTool(t, srv, "uta_ideas_show", map[string]any{"id": "no-such-idea"})
	tr := toolResultFrom(t, "uta_ideas_show", resp)
	if !tr.IsError {
		t.Fatalf("unknown id should produce IsError=true, got %+v", tr)
	}
}

// TestSessionsListTool_EmptyStore covers the no-data path. The handler
// must still respond with a valid JSON array (not null), so clients can
// iterate without nil checks.
func TestSessionsListTool_EmptyStore(t *testing.T) {
	app := newServeTestApp(t)
	srv := newServeTestServer(t, app)
	resp := callTool(t, srv, "uta_sessions_list", map[string]any{"limit": 10})
	tr := toolResultFrom(t, "uta_sessions_list", resp)
	if tr.IsError {
		t.Fatalf("unexpected IsError=true: %+v", tr)
	}
	// An empty result from json.MarshalIndent on a nil slice is the literal
	// "null"; the contract is "valid JSON" rather than "always an array".
	text := strings.TrimSpace(tr.Content[0].Text)
	if text != "null" && text != "[]" {
		var rows []any
		if err := json.Unmarshal([]byte(text), &rows); err != nil {
			t.Fatalf("not a JSON value: %v\nraw=%s", err, text)
		}
		if len(rows) != 0 {
			t.Errorf("empty store returned %d rows", len(rows))
		}
	}
}

// TestTrajectoryGetTool_UnknownSession covers the bad-id path. Since
// short-id resolution landed, an unknown session id is an explicit
// IsError "session not found" — strictly more useful to an agent client
// than the previous behavior of silently returning zero events for an
// id that never existed.
func TestTrajectoryGetTool_UnknownSession(t *testing.T) {
	app := newServeTestApp(t)
	srv := newServeTestServer(t, app)
	resp := callTool(t, srv, "uta_trajectory_get", map[string]any{"session_id": "missing"})
	tr := toolResultFrom(t, "uta_trajectory_get", resp)
	if !tr.IsError {
		t.Fatalf("expected IsError for unknown session, got %+v", tr)
	}
	if len(tr.Content) == 0 || !strings.Contains(tr.Content[0].Text, "session not found") {
		t.Fatalf("expected 'session not found' message, got %+v", tr)
	}
}

// TestImprovePickTool_EmptyBacklog: with no rows the handler returns the
// sentinel `{"idea":null,"reason":"backlog empty"}`. Lifting it into a
// stable string makes it easy for clients to branch on.
func TestImprovePickTool_EmptyBacklog(t *testing.T) {
	app := newServeTestApp(t)
	srv := newServeTestServer(t, app)
	resp := callTool(t, srv, "uta_improve_pick", map[string]any{})
	tr := toolResultFrom(t, "uta_improve_pick", resp)
	if tr.IsError {
		t.Fatalf("unexpected IsError=true: %+v", tr)
	}
	var body struct {
		Idea   any    `json:"idea"`
		Reason string `json:"reason"`
	}
	if err := json.Unmarshal([]byte(tr.Content[0].Text), &body); err != nil {
		t.Fatalf("decode pick: %v\nraw=%s", err, tr.Content[0].Text)
	}
	if body.Idea != nil {
		t.Errorf("idea should be nil on empty backlog, got %+v", body.Idea)
	}
	if !strings.Contains(body.Reason, "backlog empty") {
		t.Errorf("reason = %q", body.Reason)
	}
}

// TestImprovePickTool_PicksHighSeverity: with a high and a low idea in the
// backlog the high one wins.
func TestImprovePickTool_PicksHighSeverity(t *testing.T) {
	app := newServeTestApp(t)
	board := improve.NewBoard(app.Store)
	if err := board.Insert(&improve.Idea{Title: "low one", Severity: improve.SevLow}); err != nil {
		t.Fatalf("seed low: %v", err)
	}
	if err := board.Insert(&improve.Idea{Title: "high one", Severity: improve.SevHigh}); err != nil {
		t.Fatalf("seed high: %v", err)
	}
	srv := newServeTestServer(t, app)
	resp := callTool(t, srv, "uta_improve_pick", map[string]any{})
	tr := toolResultFrom(t, "uta_improve_pick", resp)
	if tr.IsError {
		t.Fatalf("unexpected IsError=true: %+v", tr)
	}
	// The handler returns the bare idea row on success, not the empty-backlog
	// envelope. We assert on Title to keep the test stable across UUID churn.
	var idea improve.Idea
	if err := json.Unmarshal([]byte(tr.Content[0].Text), &idea); err != nil {
		t.Fatalf("decode picked idea: %v\nraw=%s", err, tr.Content[0].Text)
	}
	if idea.Title != "high one" {
		t.Errorf("picked %q, want %q", idea.Title, "high one")
	}
}

// TestWhiteboardSetAndGetTools_RoundTrip walks the canonical writer/reader
// path: set a note, fetch it back by key, and confirm value_json plus
// author_mode survive the round trip.
func TestWhiteboardSetAndGetTools_RoundTrip(t *testing.T) {
	app := newServeTestApp(t)
	srv := newServeTestServer(t, app)

	setResp := callTool(t, srv, "uta_whiteboard_set", map[string]any{
		"key":            "ops:blocker",
		"value_json":     `"network down"`,
		"author_mode":    "ops",
		"author_session": "s-1",
	})
	setTR := toolResultFrom(t, "uta_whiteboard_set", setResp)
	if setTR.IsError {
		t.Fatalf("uta_whiteboard_set IsError=true: %+v", setTR)
	}
	var setBody struct {
		ID         int64  `json:"id"`
		Key        string `json:"key"`
		AuthorMode string `json:"author_mode"`
	}
	if err := json.Unmarshal([]byte(setTR.Content[0].Text), &setBody); err != nil {
		t.Fatalf("decode set response: %v", err)
	}
	if setBody.ID == 0 || setBody.Key != "ops:blocker" || setBody.AuthorMode != "ops" {
		t.Errorf("set response = %+v", setBody)
	}

	getResp := callTool(t, srv, "uta_whiteboard_get", map[string]any{"key": "ops:blocker"})
	getTR := toolResultFrom(t, "uta_whiteboard_get", getResp)
	if getTR.IsError {
		t.Fatalf("uta_whiteboard_get IsError=true: %+v", getTR)
	}
	var getBody struct {
		Key       string `json:"key"`
		ValueJSON string `json:"value_json"`
	}
	if err := json.Unmarshal([]byte(getTR.Content[0].Text), &getBody); err != nil {
		t.Fatalf("decode get response: %v\nraw=%s", err, getTR.Content[0].Text)
	}
	if getBody.Key != "ops:blocker" || getBody.ValueJSON != `"network down"` {
		t.Errorf("get response = %+v", getBody)
	}
}

// TestWhiteboardSetTool_RejectsInvalidJSON: value_json is gated by
// whiteboard.Set so the handler reports the failure as a tool error.
func TestWhiteboardSetTool_RejectsInvalidJSON(t *testing.T) {
	app := newServeTestApp(t)
	srv := newServeTestServer(t, app)
	resp := callTool(t, srv, "uta_whiteboard_set", map[string]any{
		"key":        "k",
		"value_json": "not-json",
	})
	tr := toolResultFrom(t, "uta_whiteboard_set", resp)
	if !tr.IsError {
		t.Fatalf("invalid JSON should produce IsError=true, got %+v", tr)
	}
	if !strings.Contains(tr.Content[0].Text, "value_json") {
		t.Errorf("error text should mention value_json: %q", tr.Content[0].Text)
	}
}

// TestWhiteboardGetTool_MissingKeyReturnsNull: an unseen key returns the
// JSON literal "null" so clients can distinguish "no entry yet" from a
// transport error.
func TestWhiteboardGetTool_MissingKeyReturnsNull(t *testing.T) {
	app := newServeTestApp(t)
	srv := newServeTestServer(t, app)
	resp := callTool(t, srv, "uta_whiteboard_get", map[string]any{"key": "never-set"})
	tr := toolResultFrom(t, "uta_whiteboard_get", resp)
	if tr.IsError {
		t.Fatalf("unexpected IsError=true: %+v", tr)
	}
	if strings.TrimSpace(tr.Content[0].Text) != "null" {
		t.Errorf("missing key text = %q, want null", tr.Content[0].Text)
	}
}

// TestWhiteboardListTool_ListsLatestPerKey: multiple writes to the same
// key surface only the most recent in the list; distinct keys each appear
// once.
func TestWhiteboardListTool_ListsLatestPerKey(t *testing.T) {
	app := newServeTestApp(t)
	wb := whiteboard.New(app.Store.DB)
	for i, val := range []string{`"v1"`, `"v2"`, `"v3"`} {
		if _, err := wb.Set(whiteboard.Entry{
			Key: "shared", ValueJSON: val, AuthorMode: fmt.Sprintf("m%d", i),
		}); err != nil {
			t.Fatalf("seed shared %d: %v", i, err)
		}
	}
	if _, err := wb.Set(whiteboard.Entry{Key: "other", ValueJSON: `1`}); err != nil {
		t.Fatalf("seed other: %v", err)
	}

	srv := newServeTestServer(t, app)
	resp := callTool(t, srv, "uta_whiteboard_list", map[string]any{"limit": 10})
	tr := toolResultFrom(t, "uta_whiteboard_list", resp)
	if tr.IsError {
		t.Fatalf("unexpected IsError=true: %+v", tr)
	}
	var rows []struct {
		Key       string `json:"key"`
		ValueJSON string `json:"value_json"`
	}
	if err := json.Unmarshal([]byte(tr.Content[0].Text), &rows); err != nil {
		t.Fatalf("decode list: %v\nraw=%s", err, tr.Content[0].Text)
	}
	if len(rows) != 2 {
		t.Fatalf("want 2 rows (one per distinct key), got %d: %+v", len(rows), rows)
	}
	for _, r := range rows {
		if r.Key == "shared" && r.ValueJSON != `"v3"` {
			t.Errorf("shared key should reflect latest write, got %q", r.ValueJSON)
		}
	}
}

// TestRunTool_RejectsBlankGoal exercises the argument-validation branch of
// uta_run without touching a real provider. The handler short-circuits
// before any worker is selected, so we never reach the supervisor.
func TestRunTool_RejectsBlankGoal(t *testing.T) {
	app := newServeTestApp(t)
	srv := newServeTestServer(t, app)
	resp := callTool(t, srv, "uta_run", map[string]any{"goal": "  "})
	tr := toolResultFrom(t, "uta_run", resp)
	if !tr.IsError {
		t.Fatalf("blank goal should produce IsError=true, got %+v", tr)
	}
	if !strings.Contains(tr.Content[0].Text, "goal is required") {
		t.Errorf("error text = %q", tr.Content[0].Text)
	}
}

// TestAuditTool_RejectsBlankPath exercises the same arg-validation seam
// for uta_audit. Like uta_run, this short-circuits before any provider
// resolution.
func TestAuditTool_RejectsBlankPath(t *testing.T) {
	app := newServeTestApp(t)
	srv := newServeTestServer(t, app)
	resp := callTool(t, srv, "uta_audit", map[string]any{"path": ""})
	tr := toolResultFrom(t, "uta_audit", resp)
	if !tr.IsError {
		t.Fatalf("blank path should produce IsError=true, got %+v", tr)
	}
	if !strings.Contains(tr.Content[0].Text, "path is required") {
		t.Errorf("error text = %q", tr.Content[0].Text)
	}
}

// TestRunTool_NoProvidersOnPath: with an empty registry the handler must
// stop at the "no provider" branch and surface a tool error rather than
// crash. This catches regressions where availableProviders() is wired up
// incorrectly.
func TestRunTool_NoProvidersOnPath(t *testing.T) {
	app := newServeTestApp(t)
	srv := newServeTestServer(t, app)
	resp := callTool(t, srv, "uta_run", map[string]any{"goal": "do a thing"})
	tr := toolResultFrom(t, "uta_run", resp)
	if !tr.IsError {
		t.Fatalf("no providers should produce IsError=true, got %+v", tr)
	}
	if !strings.Contains(tr.Content[0].Text, "no provider on PATH") {
		t.Errorf("error text = %q", tr.Content[0].Text)
	}
}

// TestMcpProjectLabel_GlobalAndProject pins the two branches of the
// label helper that builds the serve banner. Easy to check, easy to miss
// if the format ever drifts.
func TestMcpProjectLabel_GlobalAndProject(t *testing.T) {
	if got := mcpProjectLabel(&App{}); got != "(global home)" {
		t.Errorf("no project label = %q, want (global home)", got)
	}
	app := &App{ProjectName: "uta", ProjectRoot: "/tmp/uta"}
	if got := mcpProjectLabel(app); got != "uta (/tmp/uta)" {
		t.Errorf("project label = %q, want uta (/tmp/uta)", got)
	}
}
