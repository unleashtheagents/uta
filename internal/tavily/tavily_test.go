package tavily

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestClientSearchSuccessAndFormatMarkdown(t *testing.T) {
	var receivedReq SearchRequest
	var receivedAuth string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/search" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		receivedAuth = r.Header.Get("Authorization")
		if err := json.NewDecoder(r.Body).Decode(&receivedReq); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(SearchResponse{
			Query:  receivedReq.Query,
			Answer: "NVIDIA Nemotron 3 Ultra is a 550B/55B hybrid Mamba-2 + Transformer MoE model.",
			Results: []Result{
				{
					Title:   "NVIDIA Nemotron 3 Technical Report",
					URL:     "https://research.nvidia.com/nemotron-3",
					Content: "Nemotron 3 Ultra achieves 81.4% on SWE-bench Verified with 1M context window on Nebius Token Factory.",
					Score:   0.98,
				},
			},
			ResponseTime: 0.42,
		})
	}))
	defer srv.Close()

	client := &Client{
		HTTPClient: srv.Client(),
		BaseURL:    srv.URL,
		APIKey:     "tvly-test-key",
	}

	resp, err := client.Search(context.Background(), "NVIDIA Nemotron 3 Ultra benchmarks", 3)
	if err != nil {
		t.Fatalf("Search error: %v", err)
	}
	if receivedAuth != "Bearer tvly-test-key" {
		t.Errorf("Authorization = %q, want Bearer tvly-test-key", receivedAuth)
	}
	if receivedReq.MaxResults != 3 || !receivedReq.IncludeAnswer {
		t.Errorf("unexpected request payload: %+v", receivedReq)
	}
	if len(resp.Results) != 1 || resp.Results[0].Title != "NVIDIA Nemotron 3 Technical Report" {
		t.Errorf("unexpected results: %+v", resp.Results)
	}

	md := FormatMarkdown(resp)
	if !strings.Contains(md, "NVIDIA Nemotron 3 Technical Report") || !strings.Contains(md, "81.4% on SWE-bench Verified") {
		t.Errorf("FormatMarkdown missing expected content:\n%s", md)
	}
}

func TestClientSearchMissingAPIKey(t *testing.T) {
	t.Setenv("TAVILY_API_KEY", "")
	client := &Client{}
	_, err := client.Search(context.Background(), "nemotron", 5)
	if !errors.Is(err, ErrNoAPIKey) {
		t.Fatalf("expected ErrNoAPIKey, got %v", err)
	}
}
