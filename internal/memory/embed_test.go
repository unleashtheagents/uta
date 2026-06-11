package memory

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestGeminiEmbedder_HappyPath(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/models/gemini-embedding-001:batchEmbedContents" {
			t.Errorf("path = %s", r.URL.Path)
		}
		if r.URL.Query().Get("key") != "test-key" {
			t.Errorf("key = %s", r.URL.Query().Get("key"))
		}
		var body struct {
			Requests []struct {
				Model   string `json:"model"`
				Content struct {
					Parts []struct {
						Text string `json:"text"`
					} `json:"parts"`
				} `json:"content"`
			} `json:"requests"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode req: %v", err)
		}
		if len(body.Requests) != 2 {
			t.Errorf("requests = %d, want 2", len(body.Requests))
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"embeddings": []map[string]any{
				{"values": []float32{0.1, 0.2}},
				{"values": []float32{0.3, 0.4}},
			},
		})
	}))
	defer srv.Close()

	g := NewGeminiEmbedder("test-key", "")
	g.BaseURL = srv.URL
	vecs, err := g.Embed(context.Background(), []string{"hello", "world"})
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if len(vecs) != 2 || len(vecs[0]) != 2 || vecs[1][1] != 0.4 {
		t.Errorf("vecs = %v", vecs)
	}
	if g.Name() != "gemini/gemini-embedding-001" {
		t.Errorf("Name = %s", g.Name())
	}
}

func TestGeminiEmbedder_EmptyKeyErrors(t *testing.T) {
	g := NewGeminiEmbedder("", "")
	if _, err := g.Embed(context.Background(), []string{"x"}); err == nil {
		t.Fatal("expected error with empty key")
	}
}

func TestGeminiEmbedder_HTTPErrorSurfaced(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"error":"quota exceeded"}`, http.StatusTooManyRequests)
	}))
	defer srv.Close()
	g := NewGeminiEmbedder("k", "")
	g.BaseURL = srv.URL
	_, err := g.Embed(context.Background(), []string{"x"})
	if err == nil {
		t.Fatal("expected HTTP error")
	}
}

func TestOllamaEmbedder_HappyPath(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/embed" {
			t.Errorf("path = %s", r.URL.Path)
		}
		var body struct {
			Model string   `json:"model"`
			Input []string `json:"input"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body.Model != "nomic-embed-text" {
			t.Errorf("model = %s", body.Model)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"embeddings": [][]float32{{1, 2, 3}},
		})
	}))
	defer srv.Close()

	o := NewOllamaEmbedder(srv.URL, "")
	vecs, err := o.Embed(context.Background(), []string{"hi"})
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if len(vecs) != 1 || vecs[0][2] != 3 {
		t.Errorf("vecs = %v", vecs)
	}
}

func TestOllamaEmbedder_CountMismatch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"embeddings": [][]float32{}})
	}))
	defer srv.Close()
	o := NewOllamaEmbedder(srv.URL, "")
	if _, err := o.Embed(context.Background(), []string{"a", "b"}); err == nil {
		t.Fatal("expected count-mismatch error")
	}
}

func TestNewEmbedderFromEnv(t *testing.T) {
	cases := []struct {
		name string
		env  map[string]string
		want string // "" = nil embedder
	}{
		{"off", map[string]string{"UTA_EMBEDDER": "off"}, ""},
		{"unset-no-key", map[string]string{}, ""},
		{"unset-with-key", map[string]string{"GEMINI_API_KEY": "k"}, "gemini/gemini-embedding-001"},
		{"forced-gemini", map[string]string{"UTA_EMBEDDER": "gemini"}, "gemini/gemini-embedding-001"},
		{"forced-ollama", map[string]string{"UTA_EMBEDDER": "ollama"}, "ollama/nomic-embed-text"},
		{"model-override", map[string]string{"UTA_EMBEDDER": "ollama", "UTA_EMBED_MODEL": "mxbai-embed-large"}, "ollama/mxbai-embed-large"},
		{"unknown-value", map[string]string{"UTA_EMBEDDER": "wat"}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for _, k := range []string{"UTA_EMBEDDER", "UTA_EMBED_MODEL", "GEMINI_API_KEY", "OLLAMA_HOST"} {
				t.Setenv(k, "")
			}
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			e := NewEmbedderFromEnv()
			if tc.want == "" {
				if e != nil {
					t.Errorf("expected nil embedder, got %s", e.Name())
				}
				return
			}
			if e == nil {
				t.Fatalf("expected %s, got nil", tc.want)
			}
			if e.Name() != tc.want {
				t.Errorf("Name = %s, want %s", e.Name(), tc.want)
			}
		})
	}
}
