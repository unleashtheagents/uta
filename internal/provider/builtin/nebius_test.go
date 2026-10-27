package builtin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/unleashtheagents/uta/internal/provider"
)

func TestNebius_Detect(t *testing.T) {
	t.Setenv("NEBIUS_API_KEY", "")
	t.Setenv("NEBIUS_MODEL", "")
	p := &Nebius{}
	det := p.Detect(context.Background())
	if det.Available {
		t.Fatalf("expected Available=false when NEBIUS_API_KEY is unset")
	}
	if det.Version != DefaultNebiusModel {
		t.Fatalf("expected default model %q, got %q", DefaultNebiusModel, det.Version)
	}

	t.Setenv("NEBIUS_API_KEY", "nebius-test-key")
	t.Setenv("NEBIUS_MODEL", "nvidia/Nemotron-3-Ultra-550b-a55b")
	det = p.Detect(context.Background())
	if !det.Available {
		t.Fatalf("expected Available=true when NEBIUS_API_KEY is set, notes=%s", det.Notes)
	}
	if det.Version != "nvidia/Nemotron-3-Ultra-550b-a55b" {
		t.Fatalf("expected overridden model, got %q", det.Version)
	}
}

func TestNebius_ResolveAliasesAndRouter(t *testing.T) {
	cases := map[string]string{
		"ultra":     NebiusModelUltra,
		"super":     NebiusModelSuper,
		"nano":      NebiusModelNano,
		"lightning": NebiusModelLightning,
		"vl":        NebiusModelSuperVL,
		"custom/m":  "custom/m",
	}
	for in, want := range cases {
		if got := ResolveNebiusModel(in); got != want {
			t.Errorf("ResolveNebiusModel(%q) = %q, want %q", in, got, want)
		}
	}

	cat := NebiusModelCatalog()
	if len(cat) != 5 {
		t.Fatalf("expected 5 models in NebiusModelCatalog, got %d", len(cat))
	}

	r1 := RouteNebiusGoal("refactor handlers and add unit tests")
	if r1.Tier != "90/10-hybrid" || r1.WorkerModel != NebiusModelSuper || r1.PlannerModel != NebiusModelUltra {
		t.Errorf("unexpected hybrid route: %+v", r1)
	}
	r2 := RouteNebiusGoal("design formal consensus architecture")
	if r2.Tier != "ultra-deep" || r2.WorkerModel != NebiusModelUltra {
		t.Errorf("unexpected ultra route: %+v", r2)
	}
	r3 := RouteNebiusGoal("verify OWASP guardrail policy")
	if r3.Tier != "sentinel-fast" || r3.WorkerModel != NebiusModelLightning {
		t.Errorf("unexpected sentinel route: %+v", r3)
	}
	r4 := RouteNebiusGoal("inspect UI screenshot layout")
	if r4.Tier != "multimodal-vl" || r4.WorkerModel != NebiusModelSuperVL {
		t.Errorf("unexpected vl route: %+v", r4)
	}
}

func TestNebius_RunAndResumeHeadless(t *testing.T) {
	// Developer shells often carry ~/.uta/credentials.env settings; the
	// message-count assertions below assume no injected system prompt.
	t.Setenv("UTA_NEBIUS_INJECT_RULES", "")
	t.Setenv("NEBIUS_SYSTEM_PROMPT", "")
	t.Setenv("NEBIUS_MODEL", "")
	var seenReqs []nebiusChatRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer test-nebius-token" {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":{"message":"invalid api key"}}`))
			return
		}
		var req nebiusChatRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decode request: %v", err)
		}
		seenReqs = append(seenReqs, req)

		w.Header().Set("Content-Type", "application/json")
		resp := map[string]any{
			"id":    "chatcmpl-nebius-1",
			"model": req.Model,
			"choices": []map[string]any{
				{
					"index": 0,
					"message": map[string]string{
						"role":    "assistant",
						"content": "Nemotron response ready.",
					},
					"finish_reason": "stop",
				},
			},
			"usage": map[string]int64{
				"prompt_tokens":     42,
				"completion_tokens": 18,
				"total_tokens":      60,
			},
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	p := &Nebius{
		Client:  srv.Client(),
		BaseURL: srv.URL + "/v1",
		APIKey:  "test-nebius-token",
	}

	events := make(chan provider.Event, 8)
	res, err := p.RunHeadless(context.Background(), "Analyze Mamba-Transformer MoE routing", provider.RunOptions{
		Env: []string{"NEBIUS_MODEL=nvidia/Nemotron-3-Ultra-550b-a55b"},
	}, events)
	if err != nil {
		t.Fatalf("RunHeadless failed: %v", err)
	}
	if res.FinalText != "Nemotron response ready." {
		t.Fatalf("unexpected FinalText: %q", res.FinalText)
	}
	if res.TokensIn != 42 || res.TokensOut != 18 {
		t.Fatalf("unexpected usage: in=%d out=%d", res.TokensIn, res.TokensOut)
	}
	if res.SessionID == "" {
		t.Fatalf("expected non-empty SessionID")
	}

	// Resume the session and verify conversation history is preserved.
	res2, err := p.ResumeHeadless(context.Background(), res.SessionID, "Follow up question", provider.RunOptions{}, events)
	if err != nil {
		t.Fatalf("ResumeHeadless failed: %v", err)
	}
	if res2.SessionID != res.SessionID {
		t.Fatalf("expected session id %q, got %q", res.SessionID, res2.SessionID)
	}
	if len(seenReqs) != 2 {
		t.Fatalf("expected 2 requests, got %d", len(seenReqs))
	}
	if seenReqs[0].Model != "nvidia/Nemotron-3-Ultra-550b-a55b" {
		t.Fatalf("expected model override on first request, got %q", seenReqs[0].Model)
	}
	if len(seenReqs[1].Messages) != 3 {
		t.Fatalf("expected 3 messages in resumed turn (user, assistant, user), got %d", len(seenReqs[1].Messages))
	}
}

func TestNebius_SSEStreamAndWorkspaceRules(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "AGENTS.md"), []byte("Rule: always use hybrid MoE."), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "router.go"), []byte("package router\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	var gotReq nebiusChatRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotReq)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"Streamed \"}}]}\n\n"))
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"Nemotron!\"}}],\"usage\":{\"prompt_tokens\":20,\"completion_tokens\":5}}\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer srv.Close()

	p := &Nebius{
		Client:  srv.Client(),
		BaseURL: srv.URL + "/v1",
		APIKey:  "test-key",
	}
	events := make(chan provider.Event, 16)
	res, err := p.RunHeadless(context.Background(), "Inspect @router.go", provider.RunOptions{
		Workdir: dir,
		Env: []string{
			"NEBIUS_MODEL=ultra",
			"NEBIUS_STREAM=1",
			"UTA_NEBIUS_INJECT_RULES=1",
		},
	}, events)
	if err != nil {
		t.Fatalf("RunHeadless SSE failed: %v", err)
	}
	if res.FinalText != "Streamed Nemotron!" {
		t.Errorf("FinalText = %q, want %q", res.FinalText, "Streamed Nemotron!")
	}
	if gotReq.Model != NebiusModelUltra {
		t.Errorf("Model = %q, want %q", gotReq.Model, NebiusModelUltra)
	}
	if len(gotReq.Messages) != 2 || gotReq.Messages[0].Role != "system" {
		t.Fatalf("expected [system, user] messages, got %+v", gotReq.Messages)
	}
	if !strings.Contains(gotReq.Messages[0].Content, "always use hybrid MoE") {
		t.Errorf("system prompt missing AGENTS.md rule: %s", gotReq.Messages[0].Content)
	}
	if !strings.Contains(gotReq.Messages[1].Content, `<file path="router.go">`) {
		t.Errorf("user prompt missing @router.go attachment: %s", gotReq.Messages[1].Content)
	}
}

func TestNebius_ErrorClassification(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"message":"rate limit exceeded on Nebius Token Factory"}}`))
	}))
	defer srv.Close()

	p := &Nebius{
		Client:  srv.Client(),
		BaseURL: srv.URL + "/v1",
		APIKey:  "test-key",
	}
	_, err := p.RunHeadless(context.Background(), "hello", provider.RunOptions{Env: []string{"UTA_NEBIUS_MAX_RETRIES=0"}}, nil)
	if !errors.Is(err, provider.ErrQuota) {
		t.Fatalf("expected ErrQuota on 429, got %v", err)
	}
}

func TestNebius_RetriesOn429ThenSucceeds(t *testing.T) {
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		if hits <= 2 {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"error":{"message":"slow down"}}`))
			return
		}
		var req nebiusChatRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req.ServiceTier != "flex" {
			t.Errorf("service_tier = %q, want flex", req.ServiceTier)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "x", "model": req.Model,
			"choices": []map[string]any{{"index": 0, "finish_reason": "stop",
				"message": map[string]string{"role": "assistant", "content": "after retry"}}},
			"usage": map[string]int64{"prompt_tokens": 3, "completion_tokens": 2},
		})
	}))
	defer srv.Close()

	p := &Nebius{Client: srv.Client(), BaseURL: srv.URL + "/v1", APIKey: "k"}
	events := make(chan provider.Event, 32)
	res, err := p.RunHeadless(context.Background(), "hello", provider.RunOptions{
		Env: []string{"NEBIUS_SERVICE_TIER=flex", "NEBIUS_STREAM=0"},
	}, events)
	close(events)
	if err != nil || res.FinalText != "after retry" {
		t.Fatalf("expected success after two 429s, got %q err=%v", res.FinalText, err)
	}
	if hits != 3 {
		t.Errorf("hits = %d, want 3", hits)
	}
	retries := 0
	for ev := range events {
		var pl map[string]any
		_ = json.Unmarshal(ev.Payload, &pl)
		if pl["type"] == "retry" {
			retries++
		}
	}
	if retries != 2 {
		t.Errorf("retry events = %d, want 2", retries)
	}
	if d := nebiusRetryDelay(0, "2"); d != 2*time.Second {
		t.Errorf("Retry-After honoured: %v", d)
	}
	if d := nebiusRetryDelay(10, ""); d != 8*time.Second {
		t.Errorf("backoff cap: %v", d)
	}
}

func TestNebius_StreamsIncrementallyByDefault(t *testing.T) {
	var gotReq nebiusChatRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotReq)
		w.Header().Set("Content-Type", "text/event-stream")
		fl, _ := w.(http.Flusher)
		for _, chunk := range []string{
			`{"choices":[{"delta":{"reasoning_content":"hmm "}}]}`,
			`{"choices":[{"delta":{"content":"Hello"}}]}`,
			`{"choices":[{"delta":{"content":", world"}}]}`,
			`{"choices":[],"usage":{"prompt_tokens":9,"completion_tokens":4}}`,
		} {
			_, _ = fmt.Fprintf(w, "data: %s\n\n", chunk)
			if fl != nil {
				fl.Flush()
			}
			time.Sleep(100 * time.Millisecond)
		}
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer srv.Close()

	p := &Nebius{Client: srv.Client(), BaseURL: srv.URL + "/v1", APIKey: "k"}
	events := make(chan provider.Event, 32)
	var deltas []string
	var firstArrival time.Time
	done := make(chan struct{})
	go func() {
		for ev := range events {
			if ev.Kind == provider.EventAssistantText {
				if firstArrival.IsZero() {
					firstArrival = time.Now()
				}
				var pl map[string]string
				_ = json.Unmarshal(ev.Payload, &pl)
				deltas = append(deltas, pl["text"])
			}
		}
		close(done)
	}()
	start := time.Now()
	res, err := p.RunHeadless(context.Background(), "hi", provider.RunOptions{}, events)
	close(events)
	<-done
	if err != nil {
		t.Fatal(err)
	}
	if !gotReq.Stream || gotReq.StreamOptions == nil || !gotReq.StreamOptions.IncludeUsage {
		t.Errorf("default plain turn with a listener should stream with usage: %+v", gotReq)
	}
	if res.FinalText != "Hello, world" || res.TokensIn != 9 || res.TokensOut != 4 {
		t.Errorf("result = %+v", res)
	}
	if strings.Join(deltas, "") != "Hello, world" || len(deltas) < 2 {
		t.Errorf("deltas = %q (want the text in at least two live flushes)", deltas)
	}
	// The first flush must arrive while the server is still sending (it
	// takes ~400ms in total); a buffered implementation would see it at the end.
	if firstArrival.Sub(start) > 350*time.Millisecond {
		t.Errorf("first delta arrived after %v; stream was buffered, not incremental", firstArrival.Sub(start))
	}

	// Without a listener the turn is one-shot JSON unless NEBIUS_STREAM=1.
	gotReq = nebiusChatRequest{}
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotReq)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"choices": []map[string]any{{"message": map[string]string{"role": "assistant", "content": "ok"}}}})
	}))
	defer srv2.Close()
	p2 := &Nebius{Client: srv2.Client(), BaseURL: srv2.URL + "/v1", APIKey: "k"}
	if _, err := p2.RunHeadless(context.Background(), "hi", provider.RunOptions{}, nil); err != nil {
		t.Fatal(err)
	}
	if gotReq.Stream {
		t.Errorf("no listener: should not stream")
	}
}

func TestNebius_AgenticToolLoopAndFetchModels(t *testing.T) {
	dir := t.TempDir()
	callCount := 0

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":[{"id":"nvidia/Nemotron-3-Ultra-550b-a55b"},{"id":"nvidia/nemotron-3-super-120b-a12b"}]}`))
			return
		}
		var req nebiusChatRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		w.Header().Set("Content-Type", "application/json")
		if req.Model == NebiusModelLightning {
			// Sentinel guardrail check: must run with reasoning off and a
			// tiny token cap, and must not advertise tools.
			if req.ReasoningEffort != "none" || req.MaxTokens == 0 || len(req.Tools) != 0 {
				t.Errorf("unexpected sentinel request: %+v", req)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id": "sentinel", "model": req.Model,
				"choices": []map[string]any{{"index": 0, "finish_reason": "stop",
					"message": map[string]string{"role": "assistant", "content": "PASS"}}},
			})
			return
		}
		callCount++
		if len(req.Tools) == 0 {
			t.Errorf("expected tools to be advertised when PreApproveTools is set")
		}
		if callCount == 1 {
			// Round 1: Nemotron invokes write_file
			resp := map[string]any{
				"id":    "chatcmpl-tool-1",
				"model": req.Model,
				"choices": []map[string]any{
					{
						"index": 0,
						"message": map[string]any{
							"role": "assistant",
							"tool_calls": []map[string]any{
								{
									"id":   "call_write_1",
									"type": "function",
									"function": map[string]string{
										"name":      "write_file",
										"arguments": `{"path":"hello.txt","content":"nemotron agentic loop"}`,
									},
								},
							},
						},
						"finish_reason": "tool_calls",
					},
				},
				"usage": map[string]int64{"prompt_tokens": 30, "completion_tokens": 12},
			}
			_ = json.NewEncoder(w).Encode(resp)
			return
		}
		// Round 2: verify tool result is in messages and return final answer
		lastMsg := req.Messages[len(req.Messages)-1]
		if lastMsg.Role != "tool" || lastMsg.ToolCallID != "call_write_1" {
			t.Errorf("expected tool result message, got %+v", lastMsg)
		}
		resp := map[string]any{
			"id":    "chatcmpl-tool-2",
			"model": req.Model,
			"choices": []map[string]any{
				{
					"index": 0,
					"message": map[string]string{
						"role":    "assistant",
						"content": "Wrote hello.txt and verified.",
					},
					"finish_reason": "stop",
				},
			},
			"usage": map[string]int64{"prompt_tokens": 50, "completion_tokens": 10},
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	p := &Nebius{
		Client:  srv.Client(),
		BaseURL: srv.URL + "/v1",
		APIKey:  "test-key",
	}

	models, err := p.FetchModels(context.Background())
	if err != nil {
		t.Fatalf("FetchModels error: %v", err)
	}
	if len(models) != 2 || models[0] != NebiusModelUltra {
		t.Errorf("unexpected FetchModels result: %+v", models)
	}

	events := make(chan provider.Event, 16)
	res, err := p.RunHeadless(context.Background(), "Write hello.txt", provider.RunOptions{
		Workdir:         dir,
		PreApproveTools: []string{"*"},
	}, events)
	close(events)
	if err != nil {
		t.Fatalf("RunHeadless agentic error: %v", err)
	}
	if res.FinalText != "Wrote hello.txt and verified." {
		t.Errorf("FinalText = %q", res.FinalText)
	}
	written, err := os.ReadFile(filepath.Join(dir, "hello.txt"))
	if err != nil || string(written) != "nemotron agentic loop" {
		t.Errorf("expected hello.txt to be written, got %q (err=%v)", string(written), err)
	}
	var sawToolCall, sawToolResult bool
	for ev := range events {
		if ev.Kind == provider.EventToolCall {
			sawToolCall = true
		}
		if ev.Kind == provider.EventToolResult {
			sawToolResult = true
		}
	}
	if !sawToolCall || !sawToolResult {
		t.Errorf("expected EventToolCall (%v) and EventToolResult (%v)", sawToolCall, sawToolResult)
	}
}

func TestNebius_RoleTierRouting(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req nebiusChatRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		if want := nebiusReasoningEffortForModel(req.Model); req.ReasoningEffort != want {
			t.Errorf("model %s: reasoning_effort = %q, want %q", req.Model, req.ReasoningEffort, want)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "x", "model": req.Model,
			"choices": []map[string]any{{"index": 0, "finish_reason": "stop",
				"message": map[string]string{"role": "assistant", "content": req.Model}}},
			"usage": map[string]int64{"prompt_tokens": 1, "completion_tokens": 1, "total_tokens": 2},
		})
	}))
	defer srv.Close()

	cases := []struct {
		name   string
		role   provider.Role
		env    []string
		prompt string
		want   string
	}{
		{"planner->ultra", provider.RolePlanner, nil, "plan", NebiusModelUltra},
		{"synthesis->ultra", provider.RoleSynthesis, nil, "merge", NebiusModelUltra},
		{"judge->ultra", provider.RoleJudge, nil, "verify", NebiusModelUltra},
		{"worker->super", provider.RoleWorker, nil, "code", NebiusModelSuper},
		{"empty role->super", "", nil, "code", NebiusModelSuper},
		{"compact->nano", provider.RoleCompact, nil, "compact", NebiusModelNano},
		{"sentinel->lightning", provider.RoleSentinel, nil, "check", NebiusModelLightning},
		{"explicit NEBIUS_MODEL wins", provider.RolePlanner, []string{"NEBIUS_MODEL=nano"}, "plan", NebiusModelNano},
		{"routing off -> default", provider.RolePlanner, []string{"UTA_NEBIUS_ROUTING=0"}, "plan", DefaultNebiusModel},
		{"prompt directive wins", provider.RolePlanner, nil, "[model:lightning] plan", NebiusModelLightning},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			n := &Nebius{BaseURL: srv.URL + "/v1", APIKey: "k"}
			res, err := n.RunHeadless(context.Background(), tc.prompt, provider.RunOptions{Role: tc.role, Env: tc.env}, nil)
			if err != nil {
				t.Fatalf("RunHeadless: %v", err)
			}
			if res.FinalText != tc.want {
				t.Errorf("model = %q, want %q", res.FinalText, tc.want)
			}
		})
	}
}

func TestNebius_SentinelBlocksDestructiveToolCall(t *testing.T) {
	dir := t.TempDir()
	var sentinelPrompts []string
	expectBlock := true
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req nebiusChatRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		w.Header().Set("Content-Type", "application/json")
		reply := func(content string) {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id": "x", "model": req.Model,
				"choices": []map[string]any{{"index": 0, "finish_reason": "stop",
					"message": map[string]string{"role": "assistant", "content": content}}},
				"usage": map[string]int64{"prompt_tokens": 5, "completion_tokens": 1},
			})
		}
		if req.Model == NebiusModelLightning {
			sentinelPrompts = append(sentinelPrompts, req.Messages[len(req.Messages)-1].Content)
			reply("FAIL")
			return
		}
		last := req.Messages[len(req.Messages)-1]
		if last.Role == "tool" {
			if expectBlock && !strings.Contains(last.Content, "blocked by Nemotron sentinel") {
				t.Errorf("agent should see the block as the tool result, got %q", last.Content)
			}
			reply("Understood, not running that.")
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "tc", "model": req.Model,
			"choices": []map[string]any{{"index": 0, "finish_reason": "tool_calls",
				"message": map[string]any{"role": "assistant", "tool_calls": []map[string]any{{
					"id": "call_rm", "type": "function",
					"function": map[string]string{"name": "write_file", "arguments": `{"path":"owned.txt","content":"x"}`},
				}}}}},
			"usage": map[string]int64{"prompt_tokens": 5, "completion_tokens": 5},
		})
	}))
	defer srv.Close()

	p := &Nebius{Client: srv.Client(), BaseURL: srv.URL + "/v1", APIKey: "k"}
	events := make(chan provider.Event, 32)
	res, err := p.RunHeadless(context.Background(), "do the thing", provider.RunOptions{
		Workdir: dir, PreApproveTools: []string{"*"},
	}, events)
	close(events)
	if err != nil {
		t.Fatalf("RunHeadless: %v", err)
	}
	if res.FinalText != "Understood, not running that." {
		t.Errorf("FinalText = %q", res.FinalText)
	}
	if _, statErr := os.Stat(filepath.Join(dir, "owned.txt")); statErr == nil {
		t.Errorf("blocked write_file must not touch the workspace")
	}
	if len(sentinelPrompts) != 1 || !strings.Contains(sentinelPrompts[0], "write_file") {
		t.Errorf("expected one sentinel check mentioning the tool, got %q", sentinelPrompts)
	}
	var sawAlert, sawBlockedResult bool
	for ev := range events {
		switch ev.Kind {
		case provider.EventSentinelAlert:
			var pl map[string]any
			_ = json.Unmarshal(ev.Payload, &pl)
			if pl["rule"] != "nemotron_sentinel_tool_block" || pl["severity"] != "high" || pl["tool"] != "write_file" {
				t.Errorf("unexpected alert payload: %v", pl)
			}
			sawAlert = true
		case provider.EventToolResult:
			var pl map[string]any
			_ = json.Unmarshal(ev.Payload, &pl)
			if pl["sentinel"] == "fail" && pl["is_error"] == true {
				sawBlockedResult = true
			}
		}
	}
	if !sawAlert || !sawBlockedResult {
		t.Errorf("expected sentinel_alert and a failed tool_result (alert=%v result=%v)", sawAlert, sawBlockedResult)
	}

	// Disabled sentinel: the same call goes straight through.
	expectBlock = false
	events2 := make(chan provider.Event, 32)
	_, err = p.RunHeadless(context.Background(), "again", provider.RunOptions{
		Workdir: dir, PreApproveTools: []string{"*"}, Env: []string{"UTA_NEBIUS_SENTINEL=0"},
	}, events2)
	close(events2)
	if err != nil {
		t.Fatalf("RunHeadless (sentinel off): %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(dir, "owned.txt")); statErr != nil {
		t.Errorf("with the sentinel disabled the write should land: %v", statErr)
	}
}

func TestNebius_ReasoningSeparatedFromAnswer(t *testing.T) {
	var secondReq nebiusChatRequest
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		var req nebiusChatRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		if calls == 2 {
			secondReq = req
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "x", "model": req.Model,
			"choices": []map[string]any{{"index": 0, "finish_reason": "stop",
				"message": map[string]any{
					"role":              "assistant",
					"content":           "<think>scratch</think>\n\nPASS",
					"reasoning_content": "Let me weigh the options.",
				}}},
			"usage": map[string]any{
				"prompt_tokens": 20, "completion_tokens": 60,
				"completion_tokens_details": map[string]int64{"reasoning_tokens": 58},
				"prompt_tokens_details":     map[string]int64{"cached_tokens": 12},
			},
		})
	}))
	defer srv.Close()

	n := &Nebius{BaseURL: srv.URL + "/v1", APIKey: "k"}
	events := make(chan provider.Event, 32)
	res, err := n.RunHeadless(context.Background(), "verify", provider.RunOptions{Role: provider.RoleJudge}, events)
	close(events)
	if err != nil {
		t.Fatalf("RunHeadless: %v", err)
	}
	if res.FinalText != "PASS" {
		t.Errorf("FinalText = %q, want the answer without thinking", res.FinalText)
	}
	var sawReasoning, sawUsage bool
	for ev := range events {
		if ev.Kind != provider.EventStdoutChunk {
			continue
		}
		var pl map[string]any
		_ = json.Unmarshal(ev.Payload, &pl)
		switch pl["type"] {
		case "reasoning":
			sawReasoning = pl["text"] == "Let me weigh the options." && pl["reasoning_tokens"] == float64(58)
		case "usage":
			sawUsage = pl["reasoning_tokens"] == float64(58) && pl["cached_tokens"] == float64(12)
		}
	}
	if !sawReasoning || !sawUsage {
		t.Errorf("expected reasoning and usage events (reasoning=%v usage=%v)", sawReasoning, sawUsage)
	}

	// Resume: the replayed assistant turn must not carry reasoning fields.
	if _, err := n.ResumeHeadless(context.Background(), res.SessionID, "again", provider.RunOptions{}, nil); err != nil {
		t.Fatalf("ResumeHeadless: %v", err)
	}
	for _, m := range secondReq.Messages {
		if m.Role == "assistant" {
			if m.ReasoningContent != "" || m.Reasoning != "" {
				t.Errorf("reasoning replayed into history: %+v", m)
			}
			if m.Content != "PASS" {
				t.Errorf("history content = %q, want stripped answer", m.Content)
			}
		}
	}
}

func TestNebius_ReasoningEffortOverride(t *testing.T) {
	if got := resolveReasoningEffort([]string{"UTA_NEBIUS_REASONING_EFFORT=LOW"}, NebiusModelUltra, provider.RoleWorker); got != "low" {
		t.Errorf("override = %q", got)
	}
	if got := resolveReasoningEffort([]string{"UTA_NEBIUS_REASONING_EFFORT=default"}, NebiusModelUltra, provider.RoleWorker); got != "" {
		t.Errorf("default should unset the field, got %q", got)
	}
	if got := resolveReasoningEffort(nil, NebiusModelUltra, provider.RoleCompact); got != "none" {
		t.Errorf("compact role must not think, got %q", got)
	}
	if got := resolveReasoningEffort(nil, NebiusModelNano, provider.RoleWorker); got != "none" {
		t.Errorf("nano default = %q, want none", got)
	}
	if got := stripThinkTags("<THINK>a</THINK> b"); got != "b" {
		t.Errorf("stripThinkTags = %q", got)
	}
	if got := stripThinkTags("plain"); got != "plain" {
		t.Errorf("stripThinkTags(plain) = %q", got)
	}
}

func TestNebius_EmptyContentRetriesWithoutReasoning(t *testing.T) {
	var efforts []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req nebiusChatRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		efforts = append(efforts, req.ReasoningEffort)
		w.Header().Set("Content-Type", "application/json")
		content := ""
		if req.ReasoningEffort == "none" {
			content = "the digest"
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "x", "model": req.Model,
			"choices": []map[string]any{{"index": 0, "finish_reason": "length",
				"message": map[string]any{"role": "assistant", "content": content, "reasoning_content": "thinking forever"}}},
			"usage": map[string]any{"prompt_tokens": 10, "completion_tokens": 8192,
				"completion_tokens_details": map[string]int64{"reasoning_tokens": 8192}},
		})
	}))
	defer srv.Close()
	n := &Nebius{BaseURL: srv.URL + "/v1", APIKey: "k"}
	res, err := n.RunHeadless(context.Background(), "compact this", provider.RunOptions{Role: provider.RoleSynthesis, Env: []string{"NEBIUS_STREAM=0"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.FinalText != "the digest" {
		t.Errorf("FinalText = %q", res.FinalText)
	}
	if len(efforts) != 2 || efforts[0] != "high" || efforts[1] != "none" {
		t.Errorf("efforts = %v, want [high none]", efforts)
	}
}

func TestNebius_DefaultToolSetsByRole(t *testing.T) {
	names := func(specs []nebiusToolSpec) []string {
		var out []string
		for _, s := range specs {
			out = append(out, s.Function.Name)
		}
		return out
	}
	ro := names(nebiusToolsFor(provider.RunOptions{Role: provider.RoleWorker}))
	if strings.Join(ro, ",") != "read_file,list_dir,search_files,glob,tavily_search,tavily_extract" {
		t.Errorf("worker default tools = %v", ro)
	}
	if got := nebiusToolsFor(provider.RunOptions{Role: provider.RolePlanner}); got != nil {
		t.Errorf("planner must get no tools, got %v", names(got))
	}
	full := len(DefaultNebiusTools())
	if got := names(nebiusToolsFor(provider.RunOptions{PreApproveTools: []string{"*"}})); len(got) != full {
		t.Errorf("pre-approved worker gets the full set, got %v", got)
	}
	if got := names(nebiusToolsFor(provider.RunOptions{Role: provider.RoleWorker, ToolGate: allowAllGate{}})); len(got) != full {
		t.Errorf("a worker with a tool gate gets the full set and the gate decides, got %v", got)
	}
	if got := nebiusToolsFor(provider.RunOptions{Role: provider.RoleSynthesis, ToolGate: allowAllGate{}}); got != nil {
		t.Errorf("synthesis gets no tools even with a gate, got %v", names(got))
	}
	if got := nebiusToolsFor(provider.RunOptions{Env: []string{"UTA_NEBIUS_AGENTIC=0"}}); got != nil {
		t.Errorf("AGENTIC=0 disables tools, got %v", names(got))
	}
	if got := names(nebiusToolsFor(provider.RunOptions{Role: provider.RolePlanner, Env: []string{"UTA_NEBIUS_AGENTIC=1"}})); len(got) != full {
		t.Errorf("AGENTIC=1 forces the full set, got %v", got)
	}
}

type allowAllGate struct{}

func (allowAllGate) Check(context.Context, provider.ToolRequest) (provider.ToolDecision, error) {
	return provider.ToolDecision{Allow: true, Source: "test"}, nil
}
func (allowAllGate) After(context.Context, provider.ToolRequest, string, bool) string { return "" }

// recordingGate denies run_command, allows the rest, and appends feedback
// after every executed tool.
type recordingGate struct {
	mu     sync.Mutex
	checks []provider.ToolRequest
	afters []string
}

func (g *recordingGate) Check(_ context.Context, req provider.ToolRequest) (provider.ToolDecision, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.checks = append(g.checks, req)
	if req.Tool == "run_command" {
		return provider.ToolDecision{Allow: false, Reason: "commands need approval", Source: "policy"}, nil
	}
	return provider.ToolDecision{Allow: true, Source: "policy"}, nil
}

func (g *recordingGate) After(_ context.Context, req provider.ToolRequest, output string, isErr bool) string {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.afters = append(g.afters, req.Tool)
	return "hook: looks fine"
}

func TestNebius_ToolGateDeniesAndObserves(t *testing.T) {
	dir := t.TempDir()
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		var req nebiusChatRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		w.Header().Set("Content-Type", "application/json")
		switch calls {
		case 1:
			// Two tool calls: one allowed write, one denied command.
			_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"","tool_calls":[
			  {"id":"c1","type":"function","function":{"name":"write_file","arguments":"{\"path\":\"hello.txt\",\"content\":\"hi\\n\"}"}},
			  {"id":"c2","type":"function","function":{"name":"run_command","arguments":"{\"command\":\"rm -rf /\"}"}}
			]}}],"usage":{"prompt_tokens":10,"completion_tokens":5}}`))
		default:
			// Echo what the model saw for the denied call so the test can check it.
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

	gate := &recordingGate{}
	n := &Nebius{Client: srv.Client()}
	events := make(chan provider.Event, 64)
	res, err := n.RunHeadless(context.Background(), "do it", provider.RunOptions{
		Workdir:  dir,
		Env:      []string{"NEBIUS_API_KEY=test", "NEBIUS_BASE_URL=" + srv.URL, "UTA_NEBIUS_SENTINEL=0", "UTA_NEBIUS_STREAM=0", "UTA_NEBIUS_MAX_RETRIES=0"},
		ToolGate: gate,
	}, events)
	close(events)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if _, serr := os.Stat(filepath.Join(dir, "hello.txt")); serr != nil {
		t.Fatalf("allowed write_file should have run: %v", serr)
	}
	if !strings.Contains(res.FinalText, "denied by uta permissions (policy): commands need approval") {
		t.Fatalf("model should see the denial, got %q", res.FinalText)
	}
	if !strings.Contains(res.FinalText, "hook: looks fine") {
		t.Fatalf("After() feedback should reach the model, got %q", res.FinalText)
	}
	if len(gate.checks) != 2 || gate.checks[0].Summary != "hello.txt" || gate.checks[1].Summary != "rm -rf /" {
		t.Fatalf("gate checks = %+v", gate.checks)
	}
	if len(gate.afters) != 1 || gate.afters[0] != "write_file" {
		t.Fatalf("After should run only for executed tools, got %v", gate.afters)
	}
	var denied, diffs int
	for ev := range events {
		switch ev.Kind {
		case provider.EventToolDenied:
			denied++
		case provider.EventToolResult:
			if strings.Contains(string(ev.Payload), `"diff"`) && strings.Contains(string(ev.Payload), "+hi") {
				diffs++
			}
		}
	}
	if denied != 1 || diffs != 1 {
		t.Fatalf("denied=%d diffs=%d, want 1 and 1", denied, diffs)
	}
}

func TestNebius_SearchGlobTools(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "pkg", "sub"), 0o755)
	os.MkdirAll(filepath.Join(dir, ".git"), 0o755)
	os.WriteFile(filepath.Join(dir, "pkg", "a.go"), []byte("package pkg\n\nfunc Needle() {}\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "pkg", "sub", "b.go"), []byte("package sub\n// needle in a comment\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "README.md"), []byte("# needle\n"), 0o644)
	os.WriteFile(filepath.Join(dir, ".git", "config"), []byte("needle\n"), 0o644)
	opts := provider.RunOptions{Workdir: dir}

	call := func(name, args string) (string, bool) {
		var tc nebiusToolCall
		tc.Function.Name = name
		tc.Function.Arguments = args
		return executeNebiusToolCall(context.Background(), tc, opts)
	}

	out, isErr := call("search_files", `{"pattern":"(?i)needle","glob":"*.go"}`)
	if isErr || !strings.Contains(out, "pkg/a.go:3: func Needle() {}") || !strings.Contains(out, "pkg/sub/b.go:2:") || strings.Contains(out, "README") || strings.Contains(out, ".git") {
		t.Fatalf("search_files = %q err=%v", out, isErr)
	}
	out, isErr = call("search_files", `{"pattern":"needle","path":"pkg/sub"}`)
	if isErr || strings.Contains(out, "a.go") || !strings.Contains(out, "b.go") {
		t.Fatalf("scoped search = %q", out)
	}
	out, isErr = call("search_files", `{"pattern":"["}`)
	if !isErr {
		t.Fatalf("bad regexp should error, got %q", out)
	}
	out, isErr = call("glob", `{"pattern":"**/*.go"}`)
	if isErr || !strings.Contains(out, "pkg/a.go") || !strings.Contains(out, "pkg/sub/b.go") || strings.Contains(out, "README") {
		t.Fatalf("glob = %q", out)
	}
	out, isErr = call("glob", `{"pattern":"*.md"}`)
	if isErr || strings.TrimSpace(out) != "README.md" {
		t.Fatalf("glob *.md = %q", out)
	}
	out, isErr = call("glob", `{"pattern":"../*"}`)
	if !isErr {
		t.Fatalf("escaping glob should error, got %q", out)
	}
	out, isErr = call("edit_file", `{"path":"pkg/a.go","target":"c","replacement":"x"}`)
	if !isErr || !strings.Contains(out, "occurs 2 times") {
		t.Fatalf("ambiguous edit should be refused, got %q err=%v", out, isErr)
	}
}

func TestNebius_TavilyExtractTool(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/extract" {
			http.NotFound(w, r)
			return
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		urls, _ := body["urls"].([]any)
		if len(urls) != 1 || urls[0] != "https://example.com/doc" {
			t.Errorf("urls = %v", urls)
		}
		_, _ = w.Write([]byte(`{"results":[{"url":"https://example.com/doc","raw_content":"Nemotron docs body"}],"failed_results":[{"url":"https://x","error":"timeout"}]}`))
	}))
	defer srv.Close()
	var tc nebiusToolCall
	tc.Function.Name = "tavily_extract"
	tc.Function.Arguments = `{"urls":["https://example.com/doc"]}`
	out, isErr := executeNebiusToolCall(context.Background(), tc, provider.RunOptions{Env: []string{"TAVILY_API_KEY=t", "TAVILY_BASE_URL=" + srv.URL}})
	if isErr || !strings.Contains(out, "### https://example.com/doc") || !strings.Contains(out, "Nemotron docs body") || !strings.Contains(out, "could not extract https://x") {
		t.Fatalf("tavily_extract = %q err=%v", out, isErr)
	}
}

func TestNebius_StreamedToolCallsAreAssembled(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("streamed tools work"), 0o644); err != nil {
		t.Fatal(err)
	}
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		var req nebiusChatRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		if calls == 1 {
			if !req.Stream {
				t.Errorf("first turn should be streamed")
			}
			w.Header().Set("Content-Type", "text/event-stream")
			for _, chunk := range []string{
				`{"choices":[{"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"call_a","type":"function","function":{"name":"read_file","arguments":""}}]}}]}`,
				`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"path\":"}}]}}]}`,
				`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"notes.txt\"}"}}]}}]}`,
				`{"choices":[{"delta":{},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":5,"completion_tokens":9}}`,
			} {
				_, _ = fmt.Fprintf(w, "data: %s\n\n", chunk)
			}
			_, _ = w.Write([]byte("data: [DONE]\n\n"))
			return
		}
		last := req.Messages[len(req.Messages)-1]
		if last.Role != "tool" || last.ToolCallID != "call_a" || !strings.Contains(last.Content, "streamed tools work") {
			t.Errorf("tool result not replayed: %+v", last)
		}
		for _, m := range req.Messages {
			for _, tc := range m.ToolCalls {
				if tc.Index != nil {
					t.Errorf("stream index must not be replayed into history")
				}
			}
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(`data: {"choices":[{"delta":{"content":"read it"}}],"usage":{"prompt_tokens":7,"completion_tokens":2}}` + "\n\ndata: [DONE]\n\n"))
	}))
	defer srv.Close()

	p := &Nebius{Client: srv.Client(), BaseURL: srv.URL + "/v1", APIKey: "k"}
	events := make(chan provider.Event, 64)
	res, err := p.RunHeadless(context.Background(), "read notes", provider.RunOptions{Workdir: dir}, events)
	close(events)
	if err != nil {
		t.Fatal(err)
	}
	if res.FinalText != "read it" || calls != 2 {
		t.Errorf("FinalText=%q calls=%d", res.FinalText, calls)
	}
	var sawCall bool
	for ev := range events {
		if ev.Kind == provider.EventToolCall {
			sawCall = true
		}
	}
	if !sawCall {
		t.Error("expected a tool_call event from the streamed turn")
	}
}
