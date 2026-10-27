package builtin

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/unleashtheagents/uta/internal/provider"
)

// VisionResult is what Describe returns.
type VisionResult struct {
	Text      string
	Model     string
	TokensIn  int64
	TokensOut int64
	USDCents  int64
	LatencyMS int64
}

// nebiusVisionModel is the tier that reads images. UTA_NEBIUS_VISION_MODEL
// overrides it.
func nebiusVisionModel(env []string) string {
	if v := strings.TrimSpace(resolveEnvKey(env, "UTA_NEBIUS_VISION_MODEL")); v != "" {
		return v
	}
	return NebiusModelSuperVL
}

func imageMIME(path string) string {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".png":
		return "image/png"
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".gif":
		return "image/gif"
	case ".webp":
		return "image/webp"
	}
	return ""
}

// Describe sends an image plus a prompt to Nemotron Super-VL and returns
// its answer. It is the engine-free path behind `uta nebius see` and the
// shell's /image: one call, no tools, no planner.
func (n *Nebius) Describe(ctx context.Context, imagePath, prompt string, env []string) (VisionResult, error) {
	apiKey, baseURL, _ := n.resolveConfig(env)
	if apiKey == "" {
		return VisionResult{}, fmt.Errorf("nebius: %w: NEBIUS_API_KEY is not set", provider.ErrAuth)
	}
	mime := imageMIME(imagePath)
	if mime == "" {
		return VisionResult{}, fmt.Errorf("unsupported image type %q (png, jpg, gif, webp)", filepath.Ext(imagePath))
	}
	data, err := os.ReadFile(imagePath)
	if err != nil {
		return VisionResult{}, err
	}
	if len(data) > 12*1024*1024 {
		return VisionResult{}, errors.New("image larger than 12 MB")
	}
	if strings.TrimSpace(prompt) == "" {
		prompt = "Describe this image precisely. If it shows a user interface, error, diagram or code, transcribe the relevant text and point out what looks wrong or notable."
	}
	model := nebiusVisionModel(env)
	body := map[string]any{
		"model": model,
		"messages": []map[string]any{{
			"role": "user",
			"content": []map[string]any{
				{"type": "text", "text": prompt},
				{"type": "image_url", "image_url": map[string]string{"url": "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(data)}},
			},
		}},
		"max_tokens": 2048,
	}
	if effort := resolveReasoningEffort(env, model, provider.RoleWorker); effort != "" {
		body["reasoning_effort"] = effort
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return VisionResult{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/chat/completions", bytes.NewReader(raw))
	if err != nil {
		return VisionResult{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+apiKey)
	client := n.Client
	if client == nil {
		client = &http.Client{Timeout: 120 * time.Second}
	}
	start := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		return VisionResult{}, fmt.Errorf("nebius vision: %w: %v", provider.ErrTransport, err)
	}
	defer resp.Body.Close()
	respBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return VisionResult{}, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return VisionResult{}, fmt.Errorf("nebius vision (%d): %s", resp.StatusCode, strings.TrimSpace(string(respBytes)))
	}
	var parsed nebiusChatResponse
	if err := json.Unmarshal(respBytes, &parsed); err != nil {
		return VisionResult{}, errors.New("nebius vision: invalid JSON response")
	}
	res := VisionResult{Model: model, LatencyMS: time.Since(start).Milliseconds(),
		TokensIn: parsed.Usage.PromptTokens, TokensOut: parsed.Usage.CompletionTokens}
	if len(parsed.Choices) > 0 {
		res.Text = strings.TrimSpace(stripThinkTags(parsed.Choices[0].Message.Content))
	}
	res.USDCents = nebiusEstimateUSDCents(env, model, res.TokensIn, res.TokensOut)
	return res, nil
}
