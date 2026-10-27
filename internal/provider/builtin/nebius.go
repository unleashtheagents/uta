package builtin

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/unleashtheagents/uta/internal/provider"
	"github.com/unleashtheagents/uta/internal/skills"
)

const (
	// DefaultNebiusBaseURL is the OpenAI-compatible Nebius Token Factory endpoint.
	DefaultNebiusBaseURL = "https://api.tokenfactory.nebius.com/v1"
	// DefaultNebiusModel is the default NVIDIA Nemotron model on Nebius Token Factory.
	DefaultNebiusModel = "nvidia/nemotron-3-super-120b-a12b"

	// Canonical NVIDIA Nemotron model identifiers on Nebius Token Factory.
	NebiusModelUltra     = "nvidia/Nemotron-3-Ultra-550b-a55b"
	NebiusModelSuper     = "nvidia/nemotron-3-super-120b-a12b"
	NebiusModelNano      = "nvidia/NVIDIA-Nemotron-3-Nano-30B-A3B"
	NebiusModelLightning = "nvidia/Nemotron-3_5-Lightning"
	NebiusModelSuperVL   = "nvidia/Nemotron-3.5-Super-VL"
)

// NebiusModelSpec describes an NVIDIA Nemotron model hosted on Nebius Token Factory.
type NebiusModelSpec struct {
	ID             string `json:"id"`
	Alias          string `json:"alias"`
	Architecture   string `json:"architecture"`
	Params         string `json:"params"`
	ContextWindow  string `json:"context_window"`
	SWEVerified    string `json:"swe_verified"`
	ThroughputTokS int    `json:"throughput_tok_s"`
	Role           string `json:"role"`
	Description    string `json:"description"`
}

// NebiusModelCatalog returns the first-class NVIDIA Nemotron models supported by
// the Nebius Token Factory coordinator.
func NebiusModelCatalog() []NebiusModelSpec {
	return []NebiusModelSpec{
		{
			ID:             NebiusModelUltra,
			Alias:          "ultra",
			Architecture:   "Hybrid Mamba-2 + Transformer Sparse MoE",
			Params:         "550B total / 55B active",
			ContextWindow:  "1M tokens",
			SWEVerified:    "81.4%",
			ThroughputTokS: 88,
			Role:           "Planner & Synthesizer (10% tier)",
			Description:    "Frontier open-weights reasoning for DAG decomposition, adversarial synthesis, and architectural review.",
		},
		{
			ID:             NebiusModelSuper,
			Alias:          "super",
			Architecture:   "Hybrid Mamba-2 + Transformer Sparse MoE",
			Params:         "120B total / 12B active",
			ContextWindow:  "1M tokens",
			SWEVerified:    "72.8%",
			ThroughputTokS: 148,
			Role:           "Parallel Worker Swarm (90% tier)",
			Description:    "High-throughput workhorse for parallel coding subtasks, test generation, and repo-wide refactoring.",
		},
		{
			ID:             NebiusModelNano,
			Alias:          "nano",
			Architecture:   "Hybrid Mamba-2 + Transformer Sparse MoE",
			Params:         "30B total / 3B active",
			ContextWindow:  "1M tokens",
			SWEVerified:    "61.2%",
			ThroughputTokS: 295,
			Role:           "Fast Subtask & Summarizer Tier",
			Description:    "Cost-efficient 30B/3B MoE for rapid context summarization, commit synthesis, and lightweight coding loops.",
		},
		{
			ID:             NebiusModelLightning,
			Alias:          "lightning",
			Architecture:   "Latent Mamba-Attention Low-Latency",
			Params:         "30B total / 3.5B active",
			ContextWindow:  "256K tokens",
			SWEVerified:    "-",
			ThroughputTokS: 420,
			Role:           "Sentinel Guardrail & Fast Router",
			Description:    "In-loop tool-call sentinel: OWASP ASI01-ASI10 PASS/FAIL verdicts in ~300 ms measured on Nebius Token Factory (reasoning off), prompt-injection screening, real-time budget gating.",
		},
		{
			ID:             NebiusModelSuperVL,
			Alias:          "vl",
			Architecture:   "Multimodal Vision-Language MoE",
			Params:         "120B total / 12B active + ViT",
			ContextWindow:  "512K tokens",
			SWEVerified:    "70.4%",
			ThroughputTokS: 115,
			Role:           "Multimodal UI & Diagram Analyst",
			Description:    "Visual architecture diagram auditing, UI screenshot regression checks, and multimodal agentic workflows.",
		},
	}
}

// ResolveNebiusModel maps short tier aliases (ultra, super, lightning, vl) to
// their full Nebius Token Factory model identifiers, leaving custom or canonical
// identifiers untouched.
func ResolveNebiusModel(spec string) string {
	s := strings.TrimSpace(spec)
	switch strings.ToLower(s) {
	case "":
		return DefaultNebiusModel
	case "ultra", "nemotron-ultra", "nemotron-3-ultra", "550b":
		return NebiusModelUltra
	case "super", "nemotron-super", "nemotron-3-super", "120b":
		return NebiusModelSuper
	case "nano", "nemotron-nano", "nemotron-3-nano", "30b", "nvidia/nemotron-3-nano-30b-a3b":
		return NebiusModelNano
	case "lightning", "nemotron-lightning", "sentinel", "fast":
		return NebiusModelLightning
	case "vl", "vision", "super-vl", "nemotron-vl":
		return NebiusModelSuperVL
	default:
		return s
	}
}

// extractPromptModelOverride inspects a prompt for an inline `[model:<spec>]` or
// `[nebius:<spec>]` directive (e.g. `[model:ultra]`, `[model:super]`, `[model:nano]`,
// `[model:lightning]`, `[model:vl]`), strips the directive from the prompt text,
// and resolves the per-turn NVIDIA Nemotron model on Nebius Token Factory.
func extractPromptModelOverride(prompt, defaultModel string) (string, string) {
	for _, prefix := range []string{"[model:", "[nebius:"} {
		idx := strings.Index(strings.ToLower(prompt), prefix)
		if idx < 0 {
			continue
		}
		rest := prompt[idx+len(prefix):]
		end := strings.IndexByte(rest, ']')
		if end <= 0 {
			continue
		}
		spec := strings.TrimSpace(rest[:end])
		cleaned := strings.TrimSpace(prompt[:idx] + " " + rest[end+1:])
		return cleaned, ResolveNebiusModel(spec)
	}
	return prompt, defaultModel
}

// NebiusRouteDecision describes how the Nebius × NVIDIA Nemotron coordinator
// assigns models across planning, worker fan-out, and sentinel verification.
type NebiusRouteDecision struct {
	Goal          string `json:"goal"`
	Tier          string `json:"tier"`
	PlannerModel  string `json:"planner_model"`
	WorkerModel   string `json:"worker_model"`
	SentinelModel string `json:"sentinel_model"`
	Split         string `json:"split"`
	Reason        string `json:"reason"`
}

// RouteNebiusGoal classifies a goal into the optimal NVIDIA Nemotron model tier
// on Nebius Token Factory.
func RouteNebiusGoal(goal string) NebiusRouteDecision {
	low := strings.ToLower(strings.TrimSpace(goal))
	dec := NebiusRouteDecision{
		Goal:          goal,
		Tier:          "90/10-hybrid",
		PlannerModel:  NebiusModelUltra,
		WorkerModel:   NebiusModelSuper,
		SentinelModel: NebiusModelLightning,
		Split:         "90% Nemotron-3-Super (workers) / 10% Nemotron-3-Ultra (planner & synthesis)",
		Reason:        "Standard multi-agent fan-out: Nemotron-3-Ultra decomposes and synthesizes while parallel Nemotron-3-Super workers execute subtasks at 148 tok/s.",
	}
	switch {
	case strings.Contains(low, "screenshot") || strings.Contains(low, "diagram") || strings.Contains(low, "image") || strings.Contains(low, "vision") || strings.Contains(low, "ui "):
		dec.Tier = "multimodal-vl"
		dec.WorkerModel = NebiusModelSuperVL
		dec.Split = "90% Nemotron-3.5-Super-VL (vision workers) / 10% Nemotron-3-Ultra (planner)"
		dec.Reason = "Visual/UI context detected: routing worker subtasks to NVIDIA Nemotron-3.5-Super-VL with Ultra planning."
	case strings.Contains(low, "sentinel") || strings.Contains(low, "owasp") || strings.Contains(low, "guardrail") || strings.Contains(low, "classify"):
		dec.Tier = "sentinel-fast"
		dec.PlannerModel = NebiusModelSuper
		dec.WorkerModel = NebiusModelLightning
		dec.Split = "Nemotron-3.5-Lightning guardrail & policy verification (~300 ms verdicts)"
		dec.Reason = "Security/guardrail classification detected: routing to low-latency Nemotron-3.5-Lightning."
	case strings.Contains(low, "architecture") || strings.Contains(low, "formal") || strings.Contains(low, "proof") || strings.Contains(low, "consensus") || strings.Contains(low, "debate"):
		dec.Tier = "ultra-deep"
		dec.WorkerModel = NebiusModelUltra
		dec.Split = "100% Nemotron-3-Ultra-550b-a55b deep reasoning"
		dec.Reason = "High-complexity architectural/reasoning task detected: escalating workers to Nemotron-3-Ultra (550B/55B MoE)."
	}
	return dec
}

// Nebius is the built-in Nebius Token Factory provider for NVIDIA Nemotron models
// (e.g., nvidia/Nemotron-3-Ultra-550b-a55b, nvidia/nemotron-3-super-120b-a12b,
// nvidia/Nemotron-3_5-Lightning, nvidia/Nemotron-3.5-Super-VL).
//
// Configuration via environment variables (process env or RunOptions.Env):
//   - NEBIUS_API_KEY:       Bearer token for Nebius Token Factory (required)
//   - NEBIUS_MODEL:         Model identifier or alias (default: nvidia/nemotron-3-super-120b-a12b)
//   - NEBIUS_BASE_URL:      API base URL (default: https://api.tokenfactory.nebius.com/v1)
//   - NEBIUS_SYSTEM_PROMPT: Optional system prompt prepended to new sessions
//   - UTA_NEBIUS_INJECT_RULES: Set to "1" to auto-inject AGENTS.md / CLAUDE.md / GEMINI.md + skills
type Nebius struct {
	Client  *http.Client
	BaseURL string
	Model   string
	APIKey  string

	mu       sync.Mutex
	sessions map[string][]nebiusChatMessage
}

type nebiusChatMessage struct {
	Role       string           `json:"role"`
	Content    string           `json:"content"`
	ToolCalls  []nebiusToolCall `json:"tool_calls,omitempty"`
	ToolCallID string           `json:"tool_call_id,omitempty"`
	// ReasoningContent / Reasoning carry Nemotron's thinking on responses.
	// They are read from the wire and cleared before a message is replayed
	// into the next request's history.
	ReasoningContent string `json:"reasoning_content,omitempty"`
	Reasoning        string `json:"reasoning,omitempty"`
}

// thinking returns whichever reasoning field the server populated.
func (m nebiusChatMessage) thinking() string {
	if strings.TrimSpace(m.ReasoningContent) != "" {
		return m.ReasoningContent
	}
	return m.Reasoning
}

// forHistory strips response-only fields so the message can be sent back.
func (m nebiusChatMessage) forHistory() nebiusChatMessage {
	m.ReasoningContent = ""
	m.Reasoning = ""
	return m
}

// nebiusReasoningEffortForModel is the per-tier default for the
// reasoning_effort request field: Ultra plans and judges with deep thinking,
// Super and Super-VL work with moderate thinking, Nano stays light and
// Lightning answers without a thinking phase so its verdicts are not
// swamped by chain-of-thought. UTA_NEBIUS_REASONING_EFFORT overrides for
// every tier; "default" leaves the field unset so the server default applies.
func nebiusReasoningEffortForModel(model string) string {
	switch model {
	case NebiusModelUltra:
		return "high"
	case NebiusModelSuper, NebiusModelSuperVL:
		return "medium"
	case NebiusModelNano, NebiusModelLightning:
		// Measured live: Nano with reasoning on spent its entire 8192-token
		// output budget thinking and returned empty content. The small tiers
		// do transformation work (compaction, verdicts); they answer directly.
		return "none"
	}
	return ""
}

// resolveReasoningEffort picks the reasoning_effort for a call. The compact
// role always answers without a thinking phase: compaction is a rewrite, and
// a thinking budget there only competes with the digest for output tokens.
func resolveReasoningEffort(env []string, model string, role provider.Role) string {
	if v := strings.ToLower(resolveEnvKey(env, "UTA_NEBIUS_REASONING_EFFORT")); v != "" {
		if v == "default" {
			return ""
		}
		return v
	}
	if role == provider.RoleCompact || role == provider.RoleSentinel {
		return "none"
	}
	return nebiusReasoningEffortForModel(model)
}

// stripThinkTags removes a leaked <think>...</think> block that some chat
// templates place at the head of content when reasoning is on.
func stripThinkTags(text string) string {
	lower := strings.ToLower(text)
	start := strings.Index(lower, "<think>")
	if start < 0 {
		return text
	}
	end := strings.Index(lower, "</think>")
	if end < start {
		return text
	}
	return strings.TrimSpace(text[:start] + text[end+len("</think>"):])
}

type nebiusChatRequest struct {
	Model           string              `json:"model"`
	Messages        []nebiusChatMessage `json:"messages"`
	Tools           []nebiusToolSpec    `json:"tools,omitempty"`
	Stream          bool                `json:"stream,omitempty"`
	Temperature     *float64            `json:"temperature,omitempty"`
	MaxTokens       int                 `json:"max_tokens,omitempty"`
	ReasoningEffort string              `json:"reasoning_effort,omitempty"`
	ServiceTier     string              `json:"service_tier,omitempty"`
	StreamOptions   *nebiusStreamOpts   `json:"stream_options,omitempty"`
}

type nebiusStreamOpts struct {
	IncludeUsage bool `json:"include_usage"`
}

// nebiusMaxRetries is the default number of additional attempts the provider
// makes on 429 / 5xx / transport errors before giving up. The shared Nebius
// Token Factory endpoint is sized for tests, so brief 429s are normal.
const nebiusMaxRetries = 3

// nebiusRetryDelay returns how long to wait before retry attempt n (0-based),
// honouring a Retry-After header when the server sent one.
func nebiusRetryDelay(attempt int, retryAfter string) time.Duration {
	if ra := strings.TrimSpace(retryAfter); ra != "" {
		if secs, err := time.ParseDuration(ra + "s"); err == nil && secs > 0 && secs <= 60*time.Second {
			return secs
		}
	}
	d := time.Duration(500*(1<<attempt)) * time.Millisecond
	if d > 8*time.Second {
		d = 8 * time.Second
	}
	return d
}

func resolveNebiusRetries(env []string) int {
	if v := resolveEnvKey(env, "UTA_NEBIUS_MAX_RETRIES"); v != "" {
		var n int
		if _, err := fmt.Sscanf(v, "%d", &n); err == nil && n >= 0 {
			return n
		}
	}
	return nebiusMaxRetries
}

type nebiusChatResponse struct {
	ID      string `json:"id"`
	Model   string `json:"model"`
	Choices []struct {
		Index        int               `json:"index"`
		Message      nebiusChatMessage `json:"message"`
		Delta        nebiusChatMessage `json:"delta"`
		FinishReason string            `json:"finish_reason"`
	} `json:"choices"`
	Usage struct {
		PromptTokens            int64 `json:"prompt_tokens"`
		CompletionTokens        int64 `json:"completion_tokens"`
		TotalTokens             int64 `json:"total_tokens"`
		CompletionTokensDetails struct {
			ReasoningTokens int64 `json:"reasoning_tokens"`
		} `json:"completion_tokens_details"`
		PromptTokensDetails struct {
			CachedTokens int64 `json:"cached_tokens"`
		} `json:"prompt_tokens_details"`
	} `json:"usage"`
	Error *struct {
		Message string `json:"message"`
		Type    string `json:"type"`
		Code    any    `json:"code"`
	} `json:"error,omitempty"`
}

func (n *Nebius) Name() string { return "nebius" }

// NebiusModelForRole returns the Nemotron tier the coordinator assigns to an
// orchestration role when no explicit model was configured: Ultra plans,
// synthesizes and judges (the 10% tier); Super does worker fan-out (the 90%
// tier); Nano compacts; Lightning runs sentinel checks.
func NebiusModelForRole(role provider.Role) string {
	switch role {
	case provider.RolePlanner, provider.RoleSynthesis, provider.RoleJudge:
		return NebiusModelUltra
	case provider.RoleCompact:
		return NebiusModelNano
	case provider.RoleSentinel:
		return NebiusModelLightning
	default:
		return NebiusModelSuper
	}
}

// resolveConfig resolves credentials, endpoint and model. An explicitly
// configured model (Nebius.Model, NEBIUS_MODEL in the process env or in
// opts.Env) always wins. Otherwise the model follows the call's role via
// NebiusModelForRole unless UTA_NEBIUS_ROUTING=0 pins DefaultNebiusModel.
// routing reports which path chose the model: "explicit", "role" or "default".
func (n *Nebius) resolveConfig(env []string) (apiKey, baseURL, model string) {
	apiKey, baseURL, model, _ = n.resolveConfigForRole(env, "")
	return apiKey, baseURL, model
}

func (n *Nebius) resolveConfigForRole(env []string, role provider.Role) (apiKey, baseURL, model, routing string) {
	apiKey = n.APIKey
	if apiKey == "" {
		apiKey = os.Getenv("NEBIUS_API_KEY")
	}
	baseURL = n.BaseURL
	if baseURL == "" {
		baseURL = os.Getenv("NEBIUS_BASE_URL")
	}
	model = n.Model
	if model == "" {
		model = os.Getenv("NEBIUS_MODEL")
	}
	for _, kv := range env {
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		switch k {
		case "NEBIUS_API_KEY":
			if v != "" {
				apiKey = v
			}
		case "NEBIUS_BASE_URL":
			if v != "" {
				baseURL = v
			}
		case "NEBIUS_MODEL":
			if v != "" {
				model = v
			}
		}
	}
	if baseURL == "" {
		baseURL = DefaultNebiusBaseURL
	}
	baseURL = strings.TrimRight(baseURL, "/")
	routing = "explicit"
	if strings.TrimSpace(model) == "" {
		routingEnv := resolveEnvKey(env, "UTA_NEBIUS_ROUTING")
		if role != "" && routingEnv != "0" && !strings.EqualFold(routingEnv, "off") && !strings.EqualFold(routingEnv, "false") {
			model = NebiusModelForRole(role)
			routing = "role"
		} else {
			model = DefaultNebiusModel
			routing = "default"
		}
	}
	model = ResolveNebiusModel(model)
	return strings.TrimSpace(apiKey), baseURL, model, routing
}

func resolveEnvKey(env []string, key string) string {
	val := os.Getenv(key)
	for _, kv := range env {
		k, v, ok := strings.Cut(kv, "=")
		if ok && k == key && v != "" {
			val = v
		}
	}
	return strings.TrimSpace(val)
}

func (n *Nebius) Detect(_ context.Context) provider.Detection {
	apiKey, baseURL, model := n.resolveConfig(nil)
	if apiKey == "" {
		return provider.Detection{
			Available:  false,
			BinaryPath: baseURL,
			Version:    model,
			Notes:      "NEBIUS_API_KEY not set (Nebius Token Factory · NVIDIA Nemotron)",
		}
	}
	return provider.Detection{
		Available:  true,
		BinaryPath: baseURL,
		Version:    model,
		Capabilities: []provider.Capability{
			provider.CapResume,
			provider.CapStreamJSON,
			provider.CapToolPreApprove,
		},
		Notes: fmt.Sprintf("Nebius Token Factory · %s", model),
	}
}

func (n *Nebius) RunHeadless(ctx context.Context, prompt string, opts provider.RunOptions, events chan<- provider.Event) (provider.RunResult, error) {
	return n.runTurn(ctx, "", prompt, opts, events)
}

func (n *Nebius) ResumeHeadless(ctx context.Context, sessionID, prompt string, opts provider.RunOptions, events chan<- provider.Event) (provider.RunResult, error) {
	if strings.TrimSpace(sessionID) == "" {
		return provider.RunResult{}, fmt.Errorf("nebius resume: empty session id: %w", provider.ErrUnsupported)
	}
	return n.runTurn(ctx, sessionID, prompt, opts, events)
}

func (n *Nebius) runTurn(ctx context.Context, sessionID, prompt string, opts provider.RunOptions, events chan<- provider.Event) (provider.RunResult, error) {
	if opts.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, opts.Timeout)
		defer cancel()
	}

	role := opts.Role
	if role == "" {
		role = provider.RoleWorker
	}
	apiKey, baseURL, model, routing := n.resolveConfigForRole(opts.Env, role)
	if apiKey == "" {
		return provider.RunResult{ExitCode: 1}, fmt.Errorf("nebius: %w: NEBIUS_API_KEY is not set", provider.ErrAuth)
	}
	if cleaned, override := extractPromptModelOverride(prompt, model); override != model {
		prompt, model, routing = cleaned, override, "prompt"
	} else {
		prompt = cleaned
	}

	if sessionID == "" {
		sessionID = "nebius-" + uuid.NewString()
	}
	reasoningEffort := resolveReasoningEffort(opts.Env, model, role)

	// Expand @file mentions inside opts.Workdir (inspired by Google Antigravity & Claude Code).
	effectivePrompt := prompt
	if opts.Workdir != "" {
		effectivePrompt, _ = skills.ExpandFileMentions(prompt, opts.Workdir)
	}

	n.mu.Lock()
	if n.sessions == nil {
		n.sessions = make(map[string][]nebiusChatMessage)
	}
	history := append([]nebiusChatMessage(nil), n.sessions[sessionID]...)
	n.mu.Unlock()

	if len(history) == 0 {
		sysPrompt := resolveEnvKey(opts.Env, "NEBIUS_SYSTEM_PROMPT")
		if inject := resolveEnvKey(opts.Env, "UTA_NEBIUS_INJECT_RULES"); inject == "1" || strings.EqualFold(inject, "true") {
			cat := skills.Discover(opts.Workdir, "")
			if prog := cat.BuildProgressiveContext(4096); prog != "" {
				if sysPrompt != "" {
					sysPrompt += "\n\n" + prog
				} else {
					sysPrompt = prog
				}
			}
		}
		if sysPrompt != "" {
			history = append(history, nebiusChatMessage{
				Role:    "system",
				Content: sysPrompt,
			})
		}
	}

	history = append(history, nebiusChatMessage{
		Role:    "user",
		Content: effectivePrompt,
	})

	tools := nebiusToolsFor(opts)
	// Streaming: on by default whenever there is a listener, so assistant
	// text reaches the terminal as it is generated; tool-call deltas are
	// assembled from the stream. NEBIUS_STREAM=0 forces one-shot JSON;
	// NEBIUS_STREAM=1 forces SSE even without a listener.
	streamEnv := resolveEnvKey(opts.Env, "NEBIUS_STREAM")
	useStream := streamEnv == "1" || (streamEnv != "0" && events != nil)
	serviceTier := resolveEnvKey(opts.Env, "NEBIUS_SERVICE_TIER")
	maxRetries := resolveNebiusRetries(opts.Env)

	if events != nil {
		sidPayload, _ := json.Marshal(map[string]string{
			"session_id": sessionID,
			"model":      model,
			"provider":   "nebius",
			"role":       string(role),
			"routing":    routing,
			"reasoning":  reasoningEffort,
		})
		events <- provider.Event{
			Kind:      provider.EventSessionID,
			Timestamp: time.Now(),
			Payload:   sidPayload,
		}
	}

	client := n.Client
	if client == nil {
		client = http.DefaultClient
	}
	endpoint := baseURL + "/chat/completions"

	var finalText string
	var totalTokensIn, totalTokensOut int64
	var lastRawBytes []byte

	var maxTokens int
	if v := resolveEnvKey(opts.Env, "UTA_NEBIUS_MAX_TOKENS"); v != "" {
		fmt.Sscanf(v, "%d", &maxTokens)
	}
	var temperature *float64
	if v := resolveEnvKey(opts.Env, "UTA_NEBIUS_TEMPERATURE"); v != "" {
		var t float64
		if _, err := fmt.Sscanf(v, "%g", &t); err == nil {
			temperature = &t
		}
	}
	var totalReasoningTokens, totalCachedTokens int64
	reasoningRetried := false

	for round := 0; round < maxNebiusToolRounds; round++ {
		chatReq := nebiusChatRequest{
			Model:           model,
			Messages:        history,
			Tools:           tools,
			Stream:          useStream,
			Temperature:     temperature,
			MaxTokens:       maxTokens,
			ReasoningEffort: reasoningEffort,
			ServiceTier:     serviceTier,
		}
		if useStream {
			chatReq.StreamOptions = &nebiusStreamOpts{IncludeUsage: true}
		}
		reqBody, err := json.Marshal(chatReq)
		if err != nil {
			return provider.RunResult{ExitCode: 1}, fmt.Errorf("nebius marshal request: %w", err)
		}

		var (
			resp     *http.Response
			rawBytes []byte
			streamed bool
			sseText  string
			sseIn    int64
			sseOut   int64
			sseThink string
			sseCalls []nebiusToolCall
		)
		for attempt := 0; ; attempt++ {
			req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(reqBody))
			if err != nil {
				return provider.RunResult{ExitCode: 1}, fmt.Errorf("nebius build request: %w: %v", provider.ErrTransport, err)
			}
			req.Header.Set("Content-Type", "application/json")
			if useStream {
				req.Header.Set("Accept", "text/event-stream")
			} else {
				req.Header.Set("Accept", "application/json")
			}
			req.Header.Set("Authorization", "Bearer "+apiKey)

			resp, err = client.Do(req)
			if err != nil {
				if errors.Is(ctx.Err(), context.DeadlineExceeded) {
					return provider.RunResult{ExitCode: 1}, fmt.Errorf("nebius timed out: %w", provider.ErrTimeout)
				}
				if errors.Is(ctx.Err(), context.Canceled) {
					return provider.RunResult{ExitCode: 1}, ctx.Err()
				}
				if attempt < maxRetries {
					if !nebiusSleep(ctx, nebiusRetryDelay(attempt, ""), events, model, attempt+1, "transport: "+err.Error()) {
						return provider.RunResult{ExitCode: 1}, ctx.Err()
					}
					continue
				}
				return provider.RunResult{ExitCode: 1}, fmt.Errorf("nebius request failed: %w: %v", provider.ErrTransport, err)
			}

			retryable := resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500
			if retryable && attempt < maxRetries {
				body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
				_ = resp.Body.Close()
				reason := fmt.Sprintf("http %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
				if !nebiusSleep(ctx, nebiusRetryDelay(attempt, resp.Header.Get("Retry-After")), events, model, attempt+1, reason) {
					return provider.RunResult{ExitCode: 1}, ctx.Err()
				}
				continue
			}

			isSSE := resp.StatusCode >= 200 && resp.StatusCode < 300 &&
				strings.Contains(strings.ToLower(resp.Header.Get("Content-Type")), "text/event-stream")
			if isSSE {
				// Incremental: deltas reach the listener as they arrive.
				var sseReason int64
				sseText, sseThink, sseIn, sseOut, rawBytes, sseReason, sseCalls = parseNebiusSSEStream(resp.Body, model, events)
				totalReasoningTokens += sseReason
				_ = resp.Body.Close()
				streamed = true
				break
			}
			var readErr error
			rawBytes, readErr = io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if readErr != nil {
				if errors.Is(ctx.Err(), context.DeadlineExceeded) {
					return provider.RunResult{ExitCode: 1}, fmt.Errorf("nebius timed out reading response: %w", provider.ErrTimeout)
				}
				return provider.RunResult{RawOutput: rawBytes, ExitCode: 1}, fmt.Errorf("nebius read response: %w: %v", provider.ErrTransport, readErr)
			}
			break
		}
		lastRawBytes = rawBytes

		var assistantMsg nebiusChatMessage
		var roundReasoning int64
		if streamed {
			totalTokensIn += sseIn
			totalTokensOut += sseOut
			assistantMsg = nebiusChatMessage{Role: "assistant", Content: sseText, ToolCalls: sseCalls, ReasoningContent: sseThink}
		} else {
			var parsed nebiusChatResponse
			_ = json.Unmarshal(rawBytes, &parsed)

			if resp.StatusCode < 200 || resp.StatusCode >= 300 {
				errMsg := strings.TrimSpace(string(rawBytes))
				if parsed.Error != nil && parsed.Error.Message != "" {
					errMsg = parsed.Error.Message
				}
				res := provider.RunResult{
					SessionID: sessionID,
					RawOutput: rawBytes,
					ExitCode:  resp.StatusCode,
				}
				switch {
				case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
					return res, fmt.Errorf("nebius (%d): %w: %s", resp.StatusCode, provider.ErrAuth, errMsg)
				case resp.StatusCode == http.StatusTooManyRequests:
					return res, fmt.Errorf("nebius (429): %w: %s", provider.ErrQuota, errMsg)
				case resp.StatusCode >= 500:
					return res, fmt.Errorf("nebius (%d): %w: %s", resp.StatusCode, provider.ErrTransport, errMsg)
				default:
					return res, fmt.Errorf("nebius (%d): %w: %s", resp.StatusCode, provider.ErrWorkerFailed, errMsg)
				}
			}

			totalTokensIn += parsed.Usage.PromptTokens
			totalTokensOut += parsed.Usage.CompletionTokens
			roundReasoning = parsed.Usage.CompletionTokensDetails.ReasoningTokens
			totalReasoningTokens += roundReasoning
			totalCachedTokens += parsed.Usage.PromptTokensDetails.CachedTokens
			if len(parsed.Choices) > 0 {
				assistantMsg = parsed.Choices[0].Message
			}
		}
		if assistantMsg.Role == "" {
			assistantMsg.Role = "assistant"
		}
		assistantMsg.Content = stripThinkTags(assistantMsg.Content)
		if strings.TrimSpace(assistantMsg.Content) == "" && len(assistantMsg.ToolCalls) == 0 && reasoningEffort != "none" && !reasoningRetried {
			// The whole output budget went to thinking. Redo this turn once
			// with reasoning off rather than hand back an empty answer.
			reasoningRetried = true
			reasoningEffort = "none"
			if events != nil {
				pl, _ := json.Marshal(map[string]any{"type": "retry", "model": model, "attempt": 1,
					"reason": "empty content with reasoning on; retrying with reasoning_effort=none",
					"text":   "[nebius retry] empty content with reasoning on; retrying with reasoning_effort=none"})
				events <- provider.Event{Kind: provider.EventStdoutChunk, Timestamp: time.Now(), Payload: pl}
			}
			round--
			continue
		}
		if think := strings.TrimSpace(assistantMsg.thinking()); think != "" && events != nil {
			thinkPayload, _ := json.Marshal(map[string]any{
				"type":             "reasoning",
				"model":            model,
				"text":             think,
				"reasoning_tokens": roundReasoning,
			})
			events <- provider.Event{
				Kind:      provider.EventStdoutChunk,
				Timestamp: time.Now(),
				Payload:   thinkPayload,
			}
		}
		history = append(history, assistantMsg.forHistory())

		if txt := strings.TrimSpace(assistantMsg.Content); txt != "" {
			finalText = txt
			// Streamed text already reached the listener delta by delta.
			if events != nil && !streamed {
				txtPayload, _ := json.Marshal(map[string]string{
					"type":  "assistant_text",
					"model": model,
					"text":  txt,
				})
				events <- provider.Event{
					Kind:      provider.EventAssistantText,
					Timestamp: time.Now(),
					Payload:   txtPayload,
				}
			}
		}

		if len(assistantMsg.ToolCalls) == 0 {
			break
		}

		for _, tc := range assistantMsg.ToolCalls {
			if events != nil {
				tcPayload, _ := json.Marshal(map[string]any{
					"id":        tc.ID,
					"tool_name": tc.Function.Name,
					"arguments": tc.Function.Arguments,
					"model":     model,
				})
				events <- provider.Event{
					Kind:      provider.EventToolCall,
					Timestamp: time.Now(),
					Payload:   tcPayload,
				}
			}

			var toolOutput string
			var isErr bool
			var outcome nebiusToolOutcome
			var verdict sentinelVerdict
			gateReq := provider.ToolRequest{
				Tool:      tc.Function.Name,
				Arguments: tc.Function.Arguments,
				Summary:   nebiusToolSummary(tc),
				Workdir:   opts.Workdir,
			}
			gated := false
			if opts.ToolGate != nil {
				decision, gerr := opts.ToolGate.Check(ctx, gateReq)
				if gerr != nil {
					return provider.RunResult{}, fmt.Errorf("nebius tool gate: %w", gerr)
				}
				if !decision.Allow {
					gated = true
					toolOutput = fmt.Sprintf("denied by uta permissions (%s): %s", decision.Source, decision.Reason)
					isErr = true
					if events != nil {
						pl, _ := json.Marshal(map[string]any{
							"tool":      tc.Function.Name,
							"arguments": tc.Function.Arguments,
							"summary":   gateReq.Summary,
							"reason":    decision.Reason,
							"source":    decision.Source,
						})
						events <- provider.Event{Kind: provider.EventToolDenied, Timestamp: time.Now(), Payload: pl}
					}
				}
			}
			if !gated {
				verdict = n.sentinelCheck(ctx, client, apiKey, baseURL, tc, opts)
			}
			if gated {
				// nothing to execute
			} else if verdict.Blocked {
				toolOutput = fmt.Sprintf("blocked by Nemotron sentinel (%s, %d ms): %s", verdict.Model, verdict.LatencyMS, verdict.Reason)
				isErr = true
				if events != nil {
					alertPayload, _ := json.Marshal(map[string]any{
						"rule":       "nemotron_sentinel_tool_block",
						"severity":   "high",
						"message":    fmt.Sprintf("%s blocked %s: %s", verdict.Model, tc.Function.Name, verdict.Reason),
						"tool":       tc.Function.Name,
						"arguments":  tc.Function.Arguments,
						"model":      verdict.Model,
						"latency_ms": verdict.LatencyMS,
					})
					events <- provider.Event{
						Kind:      provider.EventSentinelAlert,
						Timestamp: time.Now(),
						Payload:   alertPayload,
					}
				}
			} else {
				outcome = executeNebiusTool(ctx, tc, opts)
				toolOutput, isErr = outcome.Output, outcome.IsErr
				if opts.ToolGate != nil {
					if extra := opts.ToolGate.After(ctx, gateReq, toolOutput, isErr); strings.TrimSpace(extra) != "" {
						toolOutput += "\n\n" + extra
					}
				}
			}

			if events != nil {
				trMap := map[string]any{
					"tool_call_id": tc.ID,
					"tool_name":    tc.Function.Name,
					"summary":      gateReq.Summary,
					"output":       toolOutput,
					"is_error":     isErr,
				}
				if outcome.Diff != "" {
					trMap["diff"] = outcome.Diff
					trMap["path"] = outcome.Path
				}
				if verdict.Checked {
					trMap["sentinel"] = verdict.Status
					trMap["sentinel_model"] = verdict.Model
					trMap["sentinel_ms"] = verdict.LatencyMS
				}
				trPayload, _ := json.Marshal(trMap)
				events <- provider.Event{
					Kind:      provider.EventToolResult,
					Timestamp: time.Now(),
					Payload:   trPayload,
				}
			}

			history = append(history, nebiusChatMessage{
				Role:       "tool",
				ToolCallID: tc.ID,
				Content:    toolOutput,
			})
		}
	}

	n.mu.Lock()
	n.sessions[sessionID] = history
	n.mu.Unlock()

	if totalTokensIn <= 0 {
		totalTokensIn = estimateTokens(effectivePrompt)
	}
	if totalTokensOut <= 0 {
		totalTokensOut = estimateTokens(finalText)
	}
	usd, _ := NebiusEstimateUSD(opts.Env, model, totalTokensIn, totalTokensOut)

	if events != nil {
		statPayload, _ := json.Marshal(map[string]any{
			"type":             "usage",
			"model":            model,
			"role":             string(role),
			"tokens_in":        totalTokensIn,
			"tokens_out":       totalTokensOut,
			"reasoning_tokens": totalReasoningTokens,
			"cached_tokens":    totalCachedTokens,
			"usd":              usd,
			"text":             fmt.Sprintf("[nebius usage] model=%s role=%s in=%d out=%d reasoning=%d cached=%d usd=%.6f", model, role, totalTokensIn, totalTokensOut, totalReasoningTokens, totalCachedTokens, usd),
		})
		events <- provider.Event{
			Kind:      provider.EventStdoutChunk,
			Timestamp: time.Now(),
			Payload:   statPayload,
		}
	}

	return provider.RunResult{
		SessionID:      sessionID,
		FinalText:      finalText,
		RawOutput:      lastRawBytes,
		ExitCode:       0,
		TokensIn:       totalTokensIn,
		TokensOut:      totalTokensOut,
		ApproxUSDCents: nebiusEstimateUSDCents(opts.Env, model, totalTokensIn, totalTokensOut),
	}, nil
}

// parseNebiusSSE parses a complete SSE body. Kept for callers and tests that
// already hold the bytes; the live path uses parseNebiusSSEStream.
func parseNebiusSSE(raw []byte, model string, events chan<- provider.Event) (finalText string, tokensIn, tokensOut int64) {
	text, _, in, out, _, _, _ := parseNebiusSSEStream(bytes.NewReader(raw), model, events)
	return text, in, out
}

// parseNebiusSSEStream reads an OpenAI-style event stream incrementally,
// forwarding each content delta to the listener as an assistant_text event
// the moment it arrives. Reasoning deltas are collected separately and
// returned as thinking; usage arrives on the final chunk when the request
// asked for stream_options.include_usage. raw holds every byte read so the
// caller can persist the exchange.
func parseNebiusSSEStream(r io.Reader, model string, events chan<- provider.Event) (finalText, thinking string, tokensIn, tokensOut int64, raw []byte, reasoningTokens int64, calls []nebiusToolCall) {
	var buf, think, rawBuf, pending strings.Builder
	// Tool-call deltas arrive keyed by index: the first carries id, type and
	// the function name, later ones append argument fragments.
	callByIndex := map[int]*nebiusToolCall{}
	var callOrder []int
	nextIndex := 0
	// Token deltas are coalesced into readable flushes (a line, ~80 chars,
	// or 60ms of silence) so a progress renderer that prints one line per
	// event stays legible while the text still appears as it is generated.
	lastFlush := time.Now()
	flush := func() {
		if pending.Len() == 0 || events == nil {
			return
		}
		txtPayload, _ := json.Marshal(map[string]string{
			"type":  "assistant_text",
			"model": model,
			"text":  pending.String(),
		})
		events <- provider.Event{Kind: provider.EventAssistantText, Timestamp: time.Now(), Payload: txtPayload}
		pending.Reset()
		lastFlush = time.Now()
	}
	defer flush()
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 4<<20)
	for scanner.Scan() {
		rawLine := scanner.Text()
		rawBuf.WriteString(rawLine)
		rawBuf.WriteByte('\n')
		line := strings.TrimSpace(rawLine)
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "" || payload == "[DONE]" {
			continue
		}
		var chunk nebiusChatResponse
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			continue
		}
		if chunk.Usage.PromptTokens > 0 {
			tokensIn = chunk.Usage.PromptTokens
		}
		if chunk.Usage.CompletionTokens > 0 {
			tokensOut = chunk.Usage.CompletionTokens
		}
		if chunk.Usage.CompletionTokensDetails.ReasoningTokens > 0 {
			reasoningTokens = chunk.Usage.CompletionTokensDetails.ReasoningTokens
		}
		for _, c := range chunk.Choices {
			for _, tc := range c.Delta.ToolCalls {
				idx := nextIndex
				if tc.Index != nil {
					idx = *tc.Index
				} else if tc.ID == "" && len(callOrder) > 0 {
					idx = callOrder[len(callOrder)-1] // fragment without index: continue the last call
				}
				cur, ok := callByIndex[idx]
				if !ok {
					cur = &nebiusToolCall{}
					callByIndex[idx] = cur
					callOrder = append(callOrder, idx)
					nextIndex = idx + 1
				}
				if tc.ID != "" {
					cur.ID = tc.ID
				}
				if tc.Type != "" {
					cur.Type = tc.Type
				}
				if tc.Function.Name != "" {
					cur.Function.Name = tc.Function.Name
				}
				cur.Function.Arguments += tc.Function.Arguments
			}
			if t := c.Delta.thinking(); t != "" {
				think.WriteString(t)
			} else if t := c.Message.thinking(); t != "" {
				think.WriteString(t)
			}
			delta := c.Delta.Content
			if delta == "" {
				delta = c.Message.Content
			}
			if delta != "" {
				buf.WriteString(delta)
				pending.WriteString(delta)
				switch {
				case strings.Contains(delta, "\n") || pending.Len() >= 80:
					flush()
				case time.Since(lastFlush) >= 60*time.Millisecond:
					// Time-based flush: stop at the last space so a word is
					// never split across two lines of the progress view.
					if cut := strings.LastIndexByte(pending.String(), ' '); cut > 0 {
						rest := pending.String()[cut+1:]
						head := pending.String()[:cut+1]
						pending.Reset()
						pending.WriteString(head)
						flush()
						pending.WriteString(rest)
					}
				}
			}
		}
	}
	flush()
	for _, idx := range callOrder {
		c := *callByIndex[idx]
		if c.Type == "" {
			c.Type = "function"
		}
		if c.ID == "" {
			c.ID = fmt.Sprintf("call_%d", idx)
		}
		calls = append(calls, c)
	}
	return stripThinkTags(strings.TrimSpace(buf.String())), think.String(), tokensIn, tokensOut, []byte(rawBuf.String()), reasoningTokens, calls
}

// nebiusSleep waits for the retry delay, reporting the retry on the event
// channel. It returns false when the context ended first.
func nebiusSleep(ctx context.Context, d time.Duration, events chan<- provider.Event, model string, attempt int, reason string) bool {
	if events != nil {
		pl, _ := json.Marshal(map[string]any{
			"type":           "retry",
			"model":          model,
			"attempt":        attempt,
			"retry_delay_ms": d.Milliseconds(),
			"reason":         reason,
			"text":           fmt.Sprintf("[nebius retry %d in %s] %s", attempt, d.Round(time.Millisecond), reason),
		})
		events <- provider.Event{Kind: provider.EventStdoutChunk, Timestamp: time.Now(), Payload: pl}
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
