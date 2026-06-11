// embed.go — pluggable embedding backends for the RAG layer.
//
// Provider CLIs (claude, gemini) don't expose embedding endpoints, so
// semantic indexing needs its own source. Two backends ship in v1:
//
//   - gemini: the Gemini embedding API (gemini-embedding-001) over plain
//     HTTP. Needs GEMINI_API_KEY. ~$0.15 per 1M tokens, free tier exists.
//   - ollama: a local Ollama server (default model nomic-embed-text).
//     Free, private, needs `ollama pull nomic-embed-text` once.
//
// Resolution is env-driven (NewEmbedderFromEnv) and OPTIONAL: when no
// embedder is configured the RAG layer degrades to FTS5/BM25-only
// retrieval, which needs nothing. Embedding failures at index time are
// best-effort — the document still gets full-text indexed.
package memory

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"
)

// Embedder turns text into fixed-dimension vectors. Implementations
// must be safe for concurrent use.
type Embedder interface {
	// Embed returns one vector per input, in order.
	Embed(ctx context.Context, inputs []string) ([][]float32, error)
	// Name identifies the backend+model ("gemini/gemini-embedding-001").
	// Persisted alongside vectors so a model switch can trigger reindex.
	Name() string
}

// embedHTTPTimeout caps one embedding API round trip. Indexing is
// best-effort; a hung local Ollama must not stall a run.
const embedHTTPTimeout = 30 * time.Second

// NewEmbedderFromEnv picks a backend from the environment:
//
//	UTA_EMBEDDER=gemini   force the Gemini API (errors later if no key)
//	UTA_EMBEDDER=ollama   force local Ollama
//	UTA_EMBEDDER=off      explicitly disable (returns nil)
//	unset                 auto: gemini when GEMINI_API_KEY is set, else off
//
// Model overrides: UTA_EMBED_MODEL. Ollama host: OLLAMA_HOST
// (default http://localhost:11434). Returns nil when disabled — callers
// treat nil as "FTS-only mode", never an error.
func NewEmbedderFromEnv() Embedder {
	mode := strings.ToLower(strings.TrimSpace(os.Getenv("UTA_EMBEDDER")))
	model := strings.TrimSpace(os.Getenv("UTA_EMBED_MODEL"))
	switch mode {
	case "off", "none", "disabled":
		return nil
	case "gemini":
		return NewGeminiEmbedder(os.Getenv("GEMINI_API_KEY"), model)
	case "ollama":
		return NewOllamaEmbedder(os.Getenv("OLLAMA_HOST"), model)
	case "":
		if os.Getenv("GEMINI_API_KEY") != "" {
			return NewGeminiEmbedder(os.Getenv("GEMINI_API_KEY"), model)
		}
		return nil
	default:
		// Unknown value: be conservative, don't guess a paid backend.
		return nil
	}
}

// ----- Gemini -----

const defaultGeminiEmbedModel = "gemini-embedding-001"

// GeminiEmbedder calls the Gemini batchEmbedContents endpoint with a
// plain stdlib HTTP client. BaseURL is overridable for tests.
type GeminiEmbedder struct {
	APIKey  string
	Model   string
	BaseURL string
	Client  *http.Client
}

func NewGeminiEmbedder(apiKey, model string) *GeminiEmbedder {
	if model == "" {
		model = defaultGeminiEmbedModel
	}
	return &GeminiEmbedder{
		APIKey:  apiKey,
		Model:   model,
		BaseURL: "https://generativelanguage.googleapis.com/v1beta",
		Client:  &http.Client{Timeout: embedHTTPTimeout},
	}
}

func (g *GeminiEmbedder) Name() string { return "gemini/" + g.Model }

func (g *GeminiEmbedder) Embed(ctx context.Context, inputs []string) ([][]float32, error) {
	if strings.TrimSpace(g.APIKey) == "" {
		return nil, fmt.Errorf("gemini embedder: GEMINI_API_KEY is empty")
	}
	if len(inputs) == 0 {
		return nil, nil
	}
	type part struct {
		Text string `json:"text"`
	}
	type content struct {
		Parts []part `json:"parts"`
	}
	type embedReq struct {
		Model   string  `json:"model"`
		Content content `json:"content"`
	}
	var body struct {
		Requests []embedReq `json:"requests"`
	}
	for _, in := range inputs {
		body.Requests = append(body.Requests, embedReq{
			Model:   "models/" + g.Model,
			Content: content{Parts: []part{{Text: in}}},
		})
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("gemini embedder: marshal: %w", err)
	}
	url := fmt.Sprintf("%s/models/%s:batchEmbedContents?key=%s", g.BaseURL, g.Model, g.APIKey)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := g.Client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("gemini embedder: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		var buf bytes.Buffer
		_, _ = buf.ReadFrom(resp.Body)
		return nil, fmt.Errorf("gemini embedder: HTTP %d: %s", resp.StatusCode, truncateErr(buf.String()))
	}
	var out struct {
		Embeddings []struct {
			Values []float32 `json:"values"`
		} `json:"embeddings"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("gemini embedder: decode: %w", err)
	}
	if len(out.Embeddings) != len(inputs) {
		return nil, fmt.Errorf("gemini embedder: got %d embeddings for %d inputs", len(out.Embeddings), len(inputs))
	}
	vecs := make([][]float32, len(out.Embeddings))
	for i, e := range out.Embeddings {
		vecs[i] = e.Values
	}
	return vecs, nil
}

// ----- Ollama -----

const defaultOllamaEmbedModel = "nomic-embed-text"

// OllamaEmbedder calls a local Ollama server's /api/embed endpoint.
type OllamaEmbedder struct {
	Host   string
	Model  string
	Client *http.Client
}

func NewOllamaEmbedder(host, model string) *OllamaEmbedder {
	if host == "" {
		host = "http://localhost:11434"
	}
	if model == "" {
		model = defaultOllamaEmbedModel
	}
	return &OllamaEmbedder{
		Host:   strings.TrimRight(host, "/"),
		Model:  model,
		Client: &http.Client{Timeout: embedHTTPTimeout},
	}
}

func (o *OllamaEmbedder) Name() string { return "ollama/" + o.Model }

func (o *OllamaEmbedder) Embed(ctx context.Context, inputs []string) ([][]float32, error) {
	if len(inputs) == 0 {
		return nil, nil
	}
	payload, err := json.Marshal(map[string]any{
		"model": o.Model,
		"input": inputs,
	})
	if err != nil {
		return nil, fmt.Errorf("ollama embedder: marshal: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, o.Host+"/api/embed", bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := o.Client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("ollama embedder: %w (is ollama running? `ollama pull %s`)", err, o.Model)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		var buf bytes.Buffer
		_, _ = buf.ReadFrom(resp.Body)
		return nil, fmt.Errorf("ollama embedder: HTTP %d: %s", resp.StatusCode, truncateErr(buf.String()))
	}
	var out struct {
		Embeddings [][]float32 `json:"embeddings"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("ollama embedder: decode: %w", err)
	}
	if len(out.Embeddings) != len(inputs) {
		return nil, fmt.Errorf("ollama embedder: got %d embeddings for %d inputs", len(out.Embeddings), len(inputs))
	}
	return out.Embeddings, nil
}

// truncateErr keeps API error bodies readable in wrapped errors.
func truncateErr(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 300 {
		return s[:300] + "…"
	}
	return s
}
