package builtin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/unleashtheagents/uta/internal/provider"
)

func buildMCPStub(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go toolchain not on PATH")
	}
	root, err := filepath.Abs("../../..")
	if err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(t.TempDir(), "mcp_stub_bin")
	build := exec.Command("go", "build", "-o", bin, "./tests/mcp_stub")
	build.Dir = root
	build.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build stub: %v\n%s", err, out)
	}
	return bin
}

func TestNebius_MCPToolsFromDotMCPJSON(t *testing.T) {
	bin := buildMCPStub(t)
	dir := t.TempDir()
	cfg := map[string]any{"mcpServers": map[string]any{"stub": map[string]any{"command": bin}}}
	raw, _ := json.Marshal(cfg)
	if err := os.WriteFile(filepath.Join(dir, ".mcp.json"), raw, 0o644); err != nil {
		t.Fatal(err)
	}

	calls := 0
	var advertised []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		var req nebiusChatRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		w.Header().Set("Content-Type", "application/json")
		switch calls {
		case 1:
			for _, tl := range req.Tools {
				advertised = append(advertised, tl.Function.Name)
			}
			_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"","tool_calls":[
			  {"id":"m1","type":"function","function":{"name":"mcp__stub__echo","arguments":"{\"hello\":\"nemotron\"}"}},
			  {"id":"m2","type":"function","function":{"name":"mcp__stub__boom","arguments":"{}"}}
			]}}],"usage":{"prompt_tokens":10,"completion_tokens":5}}`))
		default:
			var toolMsgs []string
			for _, m := range req.Messages {
				if m.Role == "tool" {
					toolMsgs = append(toolMsgs, m.Content)
				}
			}
			b, _ := json.Marshal(strings.Join(toolMsgs, " || "))
			_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":` + string(b) + `}}],"usage":{"prompt_tokens":10,"completion_tokens":5}}`))
		}
	}))
	defer srv.Close()

	n := &Nebius{Client: srv.Client()}
	defer n.CloseMCP()
	events := make(chan provider.Event, 128)
	res, err := n.RunHeadless(context.Background(), "use the mcp tools", provider.RunOptions{
		Workdir:  dir,
		Role:     provider.RoleWorker,
		ToolGate: allowAllGate{},
		Env:      []string{"NEBIUS_API_KEY=test", "NEBIUS_BASE_URL=" + srv.URL, "UTA_NEBIUS_SENTINEL=0", "UTA_NEBIUS_STREAM=0", "UTA_NEBIUS_MAX_RETRIES=0"},
	}, events)
	close(events)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	joined := strings.Join(advertised, ",")
	if !strings.Contains(joined, "mcp__stub__echo") || !strings.Contains(joined, "mcp__stub__boom") || !strings.Contains(joined, "read_file") {
		t.Fatalf("advertised tools = %v", advertised)
	}
	if !strings.Contains(res.FinalText, `{"hello":"nemotron"}`) {
		t.Fatalf("echo result should reach the model, got %q", res.FinalText)
	}
	var announced, results int
	for ev := range events {
		switch ev.Kind {
		case provider.EventStdoutChunk:
			if strings.Contains(string(ev.Payload), `"type":"mcp"`) && strings.Contains(string(ev.Payload), "stub") {
				announced++
			}
		case provider.EventToolResult:
			if strings.Contains(string(ev.Payload), "mcp__stub__boom") && strings.Contains(string(ev.Payload), `"is_error":true`) {
				results++
			}
		}
	}
	if announced != 1 || results != 1 {
		t.Fatalf("announced=%d boomErrors=%d", announced, results)
	}

	// Second turn in the same process: no re-announce, server reused.
	events2 := make(chan provider.Event, 128)
	calls = 0
	_, err = n.RunHeadless(context.Background(), "again", provider.RunOptions{
		Workdir: dir, Role: provider.RoleWorker, ToolGate: allowAllGate{},
		Env: []string{"NEBIUS_API_KEY=test", "NEBIUS_BASE_URL=" + srv.URL, "UTA_NEBIUS_SENTINEL=0", "UTA_NEBIUS_STREAM=0", "UTA_NEBIUS_MAX_RETRIES=0"},
	}, events2)
	close(events2)
	if err != nil {
		t.Fatal(err)
	}
	for ev := range events2 {
		if strings.Contains(string(ev.Payload), `"type":"mcp"`) {
			t.Fatalf("server should be announced once per process, got %s", ev.Payload)
		}
	}
	if len(n.mcp) != 1 {
		t.Fatalf("expected one cached connection, got %d", len(n.mcp))
	}
}

func TestNebius_MCPDisabledAndUnavailable(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, ".mcp.json"), []byte(`{"mcpServers":{"nope":{"command":"/definitely/not/here"}}}`), 0o644)
	n := &Nebius{}
	if specs := n.nebiusMCPToolSpecs(provider.RunOptions{Workdir: dir, Env: []string{"UTA_NEBIUS_MCP=0"}}, nil); specs != nil {
		t.Fatalf("UTA_NEBIUS_MCP=0 should advertise nothing, got %d", len(specs))
	}
	events := make(chan provider.Event, 8)
	if specs := n.nebiusMCPToolSpecs(provider.RunOptions{Workdir: dir}, events); len(specs) != 0 {
		t.Fatalf("broken server should advertise nothing, got %d", len(specs))
	}
	close(events)
	var sawErr bool
	for ev := range events {
		if strings.Contains(string(ev.Payload), "unavailable") {
			sawErr = true
		}
	}
	if !sawErr {
		t.Fatal("expected an unavailable notice")
	}
	if got := nebiusMCPToolName("my server", "do.thing"); got != "mcp__my_server__do_thing" {
		t.Fatalf("name = %q", got)
	}
}

func TestNebius_HistoryCompaction(t *testing.T) {
	var models []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req nebiusChatRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		models = append(models, req.Model)
		w.Header().Set("Content-Type", "application/json")
		if req.Model == NebiusModelNano {
			if len(req.Messages) != 2 || !strings.Contains(req.Messages[1].Content, "[user] turn 0") {
				t.Errorf("compaction prompt should carry the transcript, got %d msgs", len(req.Messages))
			}
			_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"The user asked many things; files a.go and b.go were edited."}}],"usage":{"prompt_tokens":5000,"completion_tokens":40}}`))
			return
		}
		// Worker turn: history must be short now.
		if len(req.Messages) > 8 {
			t.Errorf("history should be compacted, got %d messages", len(req.Messages))
		}
		if !strings.Contains(req.Messages[1].Content, "compacted by") {
			t.Errorf("second message should be the summary, got %q", req.Messages[1].Content[:40])
		}
		if req.Messages[0].Role != "system" {
			t.Errorf("system prompt must survive, got role %s", req.Messages[0].Role)
		}
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"continuing"}}],"usage":{"prompt_tokens":100,"completion_tokens":5}}`))
	}))
	defer srv.Close()

	n := &Nebius{Client: srv.Client(), sessions: map[string][]nebiusChatMessage{}}
	big := strings.Repeat("lorem ipsum dolor sit amet ", 400) // ~2.5k tokens per message
	hist := []nebiusChatMessage{{Role: "system", Content: "be terse"}}
	for i := 0; i < 20; i++ {
		hist = append(hist, nebiusChatMessage{Role: "user", Content: "turn " + strings.Repeat("0", 1) + " " + big})
		hist = append(hist, nebiusChatMessage{Role: "assistant", Content: "ok " + big})
	}
	hist[1].Content = "turn 0 " + big
	n.sessions["s1"] = hist

	events := make(chan provider.Event, 64)
	res, err := n.ResumeHeadless(context.Background(), "s1", "next question", provider.RunOptions{
		Env: []string{"NEBIUS_API_KEY=test", "NEBIUS_BASE_URL=" + srv.URL, "UTA_NEBIUS_STREAM=0", "UTA_NEBIUS_MAX_RETRIES=0", "UTA_NEBIUS_COMPACT_HISTORY_TOKENS=20000"},
	}, events)
	close(events)
	if err != nil {
		t.Fatal(err)
	}
	if res.FinalText != "continuing" {
		t.Fatalf("final = %q", res.FinalText)
	}
	if len(models) != 2 || models[0] != NebiusModelNano {
		t.Fatalf("expected Nano compaction then worker call, got %v", models)
	}
	if res.TokensIn < 5000 {
		t.Fatalf("compaction tokens should be charged, got in=%d", res.TokensIn)
	}
	var compacted bool
	for ev := range events {
		if strings.Contains(string(ev.Payload), `"type":"history_compacted"`) && strings.Contains(string(ev.Payload), `"ok":true`) {
			compacted = true
		}
	}
	if !compacted {
		t.Fatal("expected a history_compacted event")
	}
	if got := len(n.sessions["s1"]); got > 10 {
		t.Fatalf("stored history should be the compacted one, got %d messages", got)
	}
}
