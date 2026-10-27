// Package tavily provides a native, zero-dependency HTTP client for the
// Tavily Search API (https://api.tavily.com/search).
//
// Used by the Nebius × NVIDIA Nemotron coordinator (`uta nebius search` and
// the `nebius` provider's `tavily_search` tool) to ground Nemotron 3 Ultra,
// Super, and Nano reasoning in live web sources with citations.
package tavily

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

const (
	// DefaultBaseURL is the production Tavily API base URL.
	DefaultBaseURL = "https://api.tavily.com"
)

// ErrNoAPIKey is returned when TAVILY_API_KEY is not configured.
var ErrNoAPIKey = errors.New("tavily: TAVILY_API_KEY is not set")

// Client calls the Tavily Search API.
type Client struct {
	HTTPClient *http.Client
	BaseURL    string
	APIKey     string
}

// SearchRequest is the payload sent to POST /search.
type SearchRequest struct {
	APIKey        string `json:"api_key"`
	Query         string `json:"query"`
	SearchDepth   string `json:"search_depth,omitempty"`
	IncludeAnswer bool   `json:"include_answer"`
	MaxResults    int    `json:"max_results,omitempty"`
}

// Result is a single ranked document returned by Tavily.
type Result struct {
	Title   string  `json:"title"`
	URL     string  `json:"url"`
	Content string  `json:"content"`
	Score   float64 `json:"score"`
}

// SearchResponse is the structured response from Tavily POST /search.
type SearchResponse struct {
	Query        string   `json:"query"`
	Answer       string   `json:"answer,omitempty"`
	Results      []Result `json:"results"`
	ResponseTime float64  `json:"response_time,omitempty"`
}

// Search executes a runtime query against the Tavily Search API.
func (c *Client) Search(ctx context.Context, query string, maxResults int) (SearchResponse, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return SearchResponse{}, errors.New("tavily: query must not be empty")
	}
	apiKey := strings.TrimSpace(c.APIKey)
	if apiKey == "" {
		apiKey = strings.TrimSpace(os.Getenv("TAVILY_API_KEY"))
	}
	if apiKey == "" {
		return SearchResponse{}, ErrNoAPIKey
	}

	baseURL := strings.TrimSpace(c.BaseURL)
	if baseURL == "" {
		baseURL = strings.TrimSpace(os.Getenv("TAVILY_BASE_URL"))
	}
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	baseURL = strings.TrimRight(baseURL, "/")

	if maxResults <= 0 {
		maxResults = 5
	}

	reqPayload := SearchRequest{
		APIKey:        apiKey,
		Query:         query,
		SearchDepth:   "basic",
		IncludeAnswer: true,
		MaxResults:    maxResults,
	}
	bodyBytes, err := json.Marshal(reqPayload)
	if err != nil {
		return SearchResponse{}, fmt.Errorf("tavily marshal: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/search", bytes.NewReader(bodyBytes))
	if err != nil {
		return SearchResponse{}, fmt.Errorf("tavily request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+apiKey)

	hc := c.HTTPClient
	if hc == nil {
		hc = &http.Client{Timeout: 20 * time.Second}
	}

	resp, err := hc.Do(req)
	if err != nil {
		return SearchResponse{}, fmt.Errorf("tavily request failed: %w", err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return SearchResponse{}, fmt.Errorf("tavily read body: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return SearchResponse{}, fmt.Errorf("tavily (%d): %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}

	var parsed SearchResponse
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return SearchResponse{}, fmt.Errorf("tavily decode response: %w", err)
	}
	if parsed.Query == "" {
		parsed.Query = query
	}
	return parsed, nil
}

// FormatMarkdown renders a SearchResponse as a concise Markdown block suitable
// for grounding NVIDIA Nemotron models or displaying in the terminal.
func FormatMarkdown(sr SearchResponse) string {
	var b strings.Builder
	fmt.Fprintf(&b, "### Tavily Live Search: %s\n", sr.Query)
	if strings.TrimSpace(sr.Answer) != "" {
		fmt.Fprintf(&b, "\n**Direct Answer:** %s\n", strings.TrimSpace(sr.Answer))
	}
	if len(sr.Results) > 0 {
		b.WriteString("\n**Sources:**\n")
		for i, r := range sr.Results {
			snippet := strings.TrimSpace(r.Content)
			if len(snippet) > 280 {
				snippet = snippet[:277] + "..."
			}
			fmt.Fprintf(&b, "%d. [%s](%s) (score %.2f)\n   %s\n", i+1, r.Title, r.URL, r.Score, snippet)
		}
	}
	return strings.TrimSpace(b.String())
}

// ExtractResult is one page returned by Tavily Extract.
type ExtractResult struct {
	URL        string `json:"url"`
	RawContent string `json:"raw_content"`
}

// ExtractResponse is the structured response from Tavily POST /extract.
type ExtractResponse struct {
	Results []ExtractResult `json:"results"`
	Failed  []struct {
		URL   string `json:"url"`
		Error string `json:"error"`
	} `json:"failed_results,omitempty"`
	ResponseTime float64 `json:"response_time,omitempty"`
}

// Extract fetches the readable content of up to 20 URLs through the Tavily
// Extract API. It is what lets an agent read a page a search surfaced
// instead of guessing from the snippet.
func (c *Client) Extract(ctx context.Context, urls []string) (ExtractResponse, error) {
	var clean []string
	for _, u := range urls {
		if u = strings.TrimSpace(u); u != "" {
			clean = append(clean, u)
		}
	}
	if len(clean) == 0 {
		return ExtractResponse{}, errors.New("tavily: at least one url is required")
	}
	if len(clean) > 20 {
		clean = clean[:20]
	}
	apiKey := strings.TrimSpace(c.APIKey)
	if apiKey == "" {
		apiKey = strings.TrimSpace(os.Getenv("TAVILY_API_KEY"))
	}
	if apiKey == "" {
		return ExtractResponse{}, ErrNoAPIKey
	}
	baseURL := strings.TrimSpace(c.BaseURL)
	if baseURL == "" {
		baseURL = strings.TrimSpace(os.Getenv("TAVILY_BASE_URL"))
	}
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	baseURL = strings.TrimRight(baseURL, "/")

	body, err := json.Marshal(map[string]any{"api_key": apiKey, "urls": clean, "extract_depth": "basic"})
	if err != nil {
		return ExtractResponse{}, fmt.Errorf("tavily marshal: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/extract", bytes.NewReader(body))
	if err != nil {
		return ExtractResponse{}, fmt.Errorf("tavily request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+apiKey)
	hc := c.HTTPClient
	if hc == nil {
		hc = &http.Client{Timeout: 45 * time.Second}
	}
	resp, err := hc.Do(req)
	if err != nil {
		return ExtractResponse{}, fmt.Errorf("tavily extract failed: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return ExtractResponse{}, fmt.Errorf("tavily read body: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return ExtractResponse{}, fmt.Errorf("tavily extract (%d): %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	var parsed ExtractResponse
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return ExtractResponse{}, fmt.Errorf("tavily decode response: %w", err)
	}
	return parsed, nil
}

// FormatExtractMarkdown renders extracted pages for a model: one section
// per URL, each body capped at maxChars so a long page cannot flood the
// context window.
func FormatExtractMarkdown(er ExtractResponse, maxChars int) string {
	if maxChars <= 0 {
		maxChars = 12000
	}
	var b strings.Builder
	for i, r := range er.Results {
		body := strings.TrimSpace(r.RawContent)
		if len(body) > maxChars {
			body = body[:maxChars] + "\n... [truncated]"
		}
		if i > 0 {
			b.WriteString("\n\n")
		}
		fmt.Fprintf(&b, "### %s\n\n%s", r.URL, body)
	}
	for _, f := range er.Failed {
		fmt.Fprintf(&b, "\n\n(could not extract %s: %s)", f.URL, f.Error)
	}
	return strings.TrimSpace(b.String())
}
