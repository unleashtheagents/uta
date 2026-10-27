package builtin

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/unleashtheagents/uta/internal/provider"
)

// Conversation compaction. A resumed Nebius conversation keeps its full
// message history; in direct mode that grows with every turn. Once the
// history exceeds the token threshold, everything but the system prompt and
// the most recent exchanges is summarized by the compact tier (Nemotron 3
// Nano) into one message, so the expensive worker tier keeps reading a
// short, dense context. UTA_NEBIUS_COMPACT_HISTORY_TOKENS sets the
// threshold (default 40000); 0 disables.

const (
	nebiusDefaultHistoryCompactTokens = 40000
	nebiusHistoryKeepRecent           = 4
)

func nebiusHistoryCompactThreshold(env []string) int64 {
	if v := resolveEnvKey(env, "UTA_NEBIUS_COMPACT_HISTORY_TOKENS"); v != "" {
		var n int64
		if _, err := fmt.Sscanf(v, "%d", &n); err == nil {
			return n
		}
	}
	return nebiusDefaultHistoryCompactTokens
}

func nebiusHistoryTokens(history []nebiusChatMessage) int64 {
	var total int64
	for _, m := range history {
		total += estimateTokens(m.Content)
		for _, tc := range m.ToolCalls {
			total += estimateTokens(tc.Function.Arguments)
		}
	}
	return total
}

// renderTranscript turns messages into a readable transcript for the
// compaction prompt.
func renderTranscript(msgs []nebiusChatMessage) string {
	var b strings.Builder
	for _, m := range msgs {
		switch m.Role {
		case "tool":
			out := m.Content
			if len(out) > 1500 {
				out = out[:1500] + " ...[truncated]"
			}
			fmt.Fprintf(&b, "[tool result] %s\n", out)
		case "assistant":
			if strings.TrimSpace(m.Content) != "" {
				fmt.Fprintf(&b, "[assistant] %s\n", m.Content)
			}
			for _, tc := range m.ToolCalls {
				fmt.Fprintf(&b, "[assistant called %s] %s\n", tc.Function.Name, tc.Function.Arguments)
			}
		default:
			fmt.Fprintf(&b, "[%s] %s\n", m.Role, m.Content)
		}
	}
	return b.String()
}

// nebiusOneShot is a plain non-streaming chat completion used for
// side calls (history compaction).
func nebiusOneShot(ctx context.Context, client *http.Client, apiKey, baseURL, model, system, user string, maxTokens int, effort string) (string, int64, int64, error) {
	msgs := []nebiusChatMessage{}
	if system != "" {
		msgs = append(msgs, nebiusChatMessage{Role: "system", Content: system})
	}
	msgs = append(msgs, nebiusChatMessage{Role: "user", Content: user})
	body, err := json.Marshal(nebiusChatRequest{Model: model, Messages: msgs, MaxTokens: maxTokens, ReasoningEffort: effort})
	if err != nil {
		return "", 0, 0, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return "", 0, 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+apiKey)
	resp, err := client.Do(req)
	if err != nil {
		return "", 0, 0, fmt.Errorf("%w: %v", provider.ErrTransport, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return "", 0, 0, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", 0, 0, fmt.Errorf("http %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	var parsed nebiusChatResponse
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return "", 0, 0, err
	}
	if len(parsed.Choices) == 0 {
		return "", parsed.Usage.PromptTokens, parsed.Usage.CompletionTokens, fmt.Errorf("empty response")
	}
	return strings.TrimSpace(stripThinkTags(parsed.Choices[0].Message.Content)), parsed.Usage.PromptTokens, parsed.Usage.CompletionTokens, nil
}

// compactHistory summarizes the older part of history with the compact tier
// when it is over the threshold. It returns the (possibly) new history and
// the tokens the compaction itself consumed.
func (n *Nebius) compactHistory(ctx context.Context, client *http.Client, apiKey, baseURL string, history []nebiusChatMessage, opts provider.RunOptions, events chan<- provider.Event) ([]nebiusChatMessage, int64, int64) {
	threshold := nebiusHistoryCompactThreshold(opts.Env)
	if threshold <= 0 || len(history) <= nebiusHistoryKeepRecent+2 {
		return history, 0, 0
	}
	before := nebiusHistoryTokens(history)
	if before < threshold {
		return history, 0, 0
	}
	start := 0
	var system []nebiusChatMessage
	if history[0].Role == "system" {
		system = history[:1]
		start = 1
	}
	cut := len(history) - nebiusHistoryKeepRecent
	// Never split a tool call from its results.
	for cut > start && history[cut].Role == "tool" {
		cut--
	}
	if cut <= start {
		return history, 0, 0
	}
	older := history[start:cut]
	recent := history[cut:]

	_, _, model, _ := n.resolveConfigForRole(opts.Env, provider.RoleCompact)
	compactCtx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	summary, in, out, err := nebiusOneShot(compactCtx, client, apiKey, baseURL, model,
		"You compact the earlier part of a conversation between a user and a coding agent so the agent can continue without it. Keep: the user's goals and constraints, decisions made, files and symbols touched, commands run and their outcomes, open questions, and anything the agent promised to do. Drop pleasantries and repeated tool output. Write dense prose or bullets, at most 600 words.",
		"Conversation so far:\n\n"+renderTranscript(older)+"\n\nWrite the compacted summary.",
		1200, "none")
	if err != nil || summary == "" {
		if events != nil {
			pl, _ := json.Marshal(map[string]any{"type": "history_compacted", "ok": false, "error": fmt.Sprint(err),
				"text": fmt.Sprintf("[nebius compact] history compaction failed (%v); continuing with full history", err)})
			events <- provider.Event{Kind: provider.EventStdoutChunk, Timestamp: time.Now(), Payload: pl}
		}
		return history, in, out
	}
	newHistory := append([]nebiusChatMessage(nil), system...)
	newHistory = append(newHistory,
		nebiusChatMessage{Role: "user", Content: "Summary of the conversation so far (compacted by " + model + "):\n\n" + summary},
		nebiusChatMessage{Role: "assistant", Content: "Understood. I will continue from that summary."},
	)
	newHistory = append(newHistory, recent...)
	after := nebiusHistoryTokens(newHistory)
	if events != nil {
		pl, _ := json.Marshal(map[string]any{"type": "history_compacted", "ok": true, "model": model,
			"messages_before": len(history), "messages_after": len(newHistory), "tokens_before": before, "tokens_after": after,
			"text": fmt.Sprintf("[nebius compact] %s compacted %d messages (~%d tokens) into a summary (~%d tokens)", model, len(older), before, after)})
		events <- provider.Event{Kind: provider.EventStdoutChunk, Timestamp: time.Now(), Payload: pl}
	}
	return newHistory, in, out
}
