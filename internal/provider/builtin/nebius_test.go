package builtin

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

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
	_, err := p.RunHeadless(context.Background(), "hello", provider.RunOptions{}, nil)
	if !errors.Is(err, provider.ErrQuota) {
		t.Fatalf("expected ErrQuota on 429, got %v", err)
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
		callCount++
		var req nebiusChatRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		if len(req.Tools) == 0 {
			t.Errorf("expected tools to be advertised when PreApproveTools is set")
		}
		w.Header().Set("Content-Type", "application/json")
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
