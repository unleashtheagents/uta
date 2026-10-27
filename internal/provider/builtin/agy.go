package builtin

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/unleashtheagents/uta/internal/provider"
)

// Agy is the Google Antigravity CLI (agy) provider.
//
// Invocation: agy -p "<prompt>" --output-format stream-json [--conversation <id>]
type Agy struct {
	pathOnce sync.Once
	path     string
	pathErr  error
}

func (a *Agy) Name() string { return "agy" }

// resolvePath looks up the absolute path to `agy` exactly once and caches the result.
func (a *Agy) resolvePath() (string, error) {
	a.pathOnce.Do(func() {
		a.path, a.pathErr = exec.LookPath("agy")
	})
	return a.path, a.pathErr
}

var agyVersionRE = regexp.MustCompile(`(\d+\.\d+(?:\.\d+)?)`)

func (a *Agy) Detect(ctx context.Context) provider.Detection {
	path, err := a.resolvePath()
	if err != nil {
		return provider.Detection{
			Available: false,
			Notes:     "binary 'agy' (Google Antigravity CLI) not found on PATH",
			Err:       fmt.Errorf("look path 'agy': %w", err),
		}
	}
	out, err := exec.CommandContext(ctx, path, "--version").CombinedOutput()
	if err != nil {
		return provider.Detection{
			Available:  false,
			BinaryPath: path,
			Notes:      fmt.Sprintf("'agy --version' failed: %v", err),
			Err:        fmt.Errorf("agy --version: %w", err),
		}
	}
	version := ""
	if m := agyVersionRE.FindString(strings.TrimSpace(string(out))); m != "" {
		version = m
	}
	return provider.Detection{
		Available:  true,
		BinaryPath: path,
		Version:    version,
		Capabilities: []provider.Capability{
			provider.CapResume,
			provider.CapStreamJSON,
			provider.CapMCP,
			provider.CapToolPreApprove,
		},
		Notes: "Google Antigravity CLI (AGY)",
	}
}

func (a *Agy) RunHeadless(ctx context.Context, prompt string, opts provider.RunOptions, events chan<- provider.Event) (provider.RunResult, error) {
	return a.runHeadless(ctx, prompt, "", opts, events)
}

func buildAgyArgs(prompt, resumeID string, opts provider.RunOptions) []string {
	args := []string{"-p", prompt, "--output-format", "stream-json"}
	if resumeID != "" {
		args = append(args, "--conversation", resumeID)
	}
	if len(opts.PreApproveTools) > 0 {
		args = append(args, "--allowed-tools", strings.Join(opts.PreApproveTools, ","))
	}
	if opts.MCPConfigPath != "" {
		args = append(args, "--mcp-config", opts.MCPConfigPath)
	}
	args = append(args, opts.ExtraArgs...)
	return args
}

func (a *Agy) ResumeHeadless(ctx context.Context, sessionID, prompt string, opts provider.RunOptions, events chan<- provider.Event) (provider.RunResult, error) {
	if sessionID == "" {
		return provider.RunResult{}, fmt.Errorf("agy resume: empty session id: %w", provider.ErrUnsupported)
	}
	return a.runHeadless(ctx, prompt, sessionID, opts, events)
}

func (a *Agy) runHeadless(ctx context.Context, prompt, resumeID string, opts provider.RunOptions, events chan<- provider.Event) (provider.RunResult, error) {
	args := buildAgyArgs(prompt, resumeID, opts)

	if opts.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, opts.Timeout)
		defer cancel()
	}

	binPath, err := a.resolvePath()
	if err != nil {
		return provider.RunResult{}, fmt.Errorf("agy: %w: binary 'agy' not found on PATH", provider.ErrTransport)
	}
	cmd := exec.CommandContext(ctx, binPath, args...)
	if opts.Workdir != "" {
		cmd.Dir = opts.Workdir
	}
	cmd.Env = append(cmd.Environ(), "AGY_TRUST_WORKSPACE=true", "GEMINI_CLI_TRUST_WORKSPACE=true")
	if len(opts.Env) > 0 {
		cmd.Env = append(cmd.Env, opts.Env...)
	}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return provider.RunResult{}, fmt.Errorf("agy stdout pipe: %w", provider.ErrTransport)
	}
	var stderrBuf bytes.Buffer
	cmd.Stderr = &stderrBuf

	if err := cmd.Start(); err != nil {
		return provider.RunResult{}, fmt.Errorf("agy start: %w: %v", provider.ErrTransport, err)
	}

	var (
		rawBuf bytes.Buffer
		rawMu  sync.Mutex
	)
	tee := io.TeeReader(stdout, &lockedWriter{mu: &rawMu, w: &rawBuf})

	res, scanErr := parseAgyStreamWithErr(tee, events)
	waitErr := cmd.Wait()
	exit := 0
	if exitErr, ok := waitErr.(*exec.ExitError); ok {
		exit = exitErr.ExitCode()
	}

	finalAnswer := strings.TrimSpace(res.FinalText)
	tokensIn := res.TokensIn
	if tokensIn <= 0 {
		tokensIn = estimateTokens(prompt)
	}
	tokensOut := res.TokensOut
	if tokensOut <= 0 {
		tokensOut = estimateTokens(finalAnswer)
	}
	inRate, outRate := agyPriceFromEnv()
	result := provider.RunResult{
		SessionID:      res.SessionID,
		FinalText:      finalAnswer,
		RawOutput:      rawBuf.Bytes(),
		ExitCode:       exit,
		TokensIn:       tokensIn,
		TokensOut:      tokensOut,
		ApproxUSDCents: estimateUSDCents(tokensIn, tokensOut, inRate, outRate),
	}

	if waitErr != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return result, fmt.Errorf("agy timed out: %w", provider.ErrTimeout)
		}
		if errors.Is(ctx.Err(), context.Canceled) {
			return result, ctx.Err()
		}
		stderr := strings.TrimSpace(stderrBuf.String())
		low := strings.ToLower(stderr)
		switch {
		case strings.Contains(low, "quota") ||
			strings.Contains(low, "rate limit") ||
			strings.Contains(low, "ratelimit") ||
			strings.Contains(low, "rate_limit") ||
			strings.Contains(low, "resource_exhausted") ||
			strings.Contains(low, "resource exhausted") ||
			strings.Contains(low, "capacity_exhausted") ||
			strings.Contains(low, "model_capacity") ||
			strings.Contains(low, "no capacity available") ||
			strings.Contains(low, "too many requests") ||
			strings.Contains(low, `"code": 429`) ||
			strings.Contains(low, `"code":429`) ||
			strings.Contains(low, "status 429"):
			return result, fmt.Errorf("agy: %w: %s", provider.ErrQuota, stderr)
		case strings.Contains(low, "unauthorized") ||
			strings.Contains(low, "invalid api key") ||
			strings.Contains(low, "invalid_api_key") ||
			strings.Contains(low, "authentication failed") ||
			strings.Contains(low, "api key not valid") ||
			strings.Contains(low, "permission denied") ||
			strings.Contains(low, `"code": 401`) ||
			strings.Contains(low, `"code": 403`):
			return result, fmt.Errorf("agy: %w: %s", provider.ErrAuth, stderr)
		case exit != 0:
			return result, fmt.Errorf("agy exit %d: %w: %s", exit, provider.ErrWorkerFailed, stderr)
		default:
			return result, fmt.Errorf("agy: %w: %v: %s", provider.ErrTransport, waitErr, stderr)
		}
	}
	if scanErr != nil {
		return result, fmt.Errorf("agy stream parse: %w: %v", provider.ErrTransport, scanErr)
	}
	return result, nil
}

type agyEvent struct {
	Type         string          `json:"type"`
	Role         string          `json:"role,omitempty"`
	SessionID    string          `json:"session_id,omitempty"`
	Conversation string          `json:"conversation_id,omitempty"`
	Text         string          `json:"text,omitempty"`
	Content      string          `json:"content,omitempty"`
	Delta        string          `json:"delta,omitempty"`
	Result       string          `json:"result,omitempty"`
	Message      string          `json:"message,omitempty"`
	Error        string          `json:"error,omitempty"`
	Tool         string          `json:"tool,omitempty"`
	Name         string          `json:"name,omitempty"`
	CallID       string          `json:"call_id,omitempty"`
	Args         json.RawMessage `json:"args,omitempty"`
	ToolResult   json.RawMessage `json:"tool_result,omitempty"`
	InputTokens  int64           `json:"input_tokens,omitempty"`
	OutputTokens int64           `json:"output_tokens,omitempty"`
}

func parseAgyStream(r io.Reader, events chan<- provider.Event) provider.RunResult {
	res, _ := parseAgyStreamWithErr(r, events)
	return res
}

func parseAgyStreamWithErr(r io.Reader, events chan<- provider.Event) (provider.RunResult, error) {
	var res provider.RunResult
	var textBuf strings.Builder
	scanner := bufio.NewScanner(r)
	buf := make([]byte, 64*1024)
	scanner.Buffer(buf, 16*1024*1024)

	for scanner.Scan() {
		rawLine := bytes.TrimSpace(scanner.Bytes())
		if len(rawLine) == 0 {
			continue
		}
		// Copy line bytes so scanner buffer reuse never aliases Event.Payload across goroutines.
		line := append([]byte(nil), rawLine...)
		var evt agyEvent
		if err := json.Unmarshal(line, &evt); err != nil {
			safeEmit(events, provider.Event{
				Kind:      provider.EventStdoutChunk,
				Timestamp: time.Now(),
				Payload:   json.RawMessage(line),
			})
			continue
		}
		sid := evt.SessionID
		if sid == "" {
			sid = evt.Conversation
		}
		if sid != "" {
			if res.SessionID == "" {
				res.SessionID = sid
			}
			payload, _ := json.Marshal(map[string]string{"session_id": sid})
			safeEmit(events, provider.Event{
				Kind:      provider.EventSessionID,
				Timestamp: time.Now(),
				Payload:   payload,
			})
		}
		if evt.InputTokens > 0 {
			res.TokensIn += evt.InputTokens
		}
		if evt.OutputTokens > 0 {
			res.TokensOut += evt.OutputTokens
		}

		switch strings.ToLower(evt.Type) {
		case "init":
			// Session ID already handled above.
		case "assistant", "assistant_text", "text", "delta":
			chunk := evt.Delta
			if chunk == "" {
				chunk = evt.Text
			}
			if chunk == "" {
				chunk = evt.Content
			}
			if chunk != "" {
				textBuf.WriteString(chunk)
				payload, _ := json.Marshal(map[string]string{"text": chunk})
				safeEmit(events, provider.Event{
					Kind:      provider.EventAssistantText,
					Timestamp: time.Now(),
					Payload:   payload,
				})
			}
		case "message":
			if strings.ToLower(evt.Role) != "assistant" {
				continue
			}
			chunk := evt.Content
			if chunk == "" {
				chunk = evt.Text
			}
			if chunk != "" {
				textBuf.WriteString(chunk)
				payload, _ := json.Marshal(map[string]string{"text": chunk})
				safeEmit(events, provider.Event{
					Kind:      provider.EventAssistantText,
					Timestamp: time.Now(),
					Payload:   payload,
				})
			}
		case "tool_call", "tool_use":
			safeEmit(events, provider.Event{
				Kind:      provider.EventToolCall,
				Timestamp: time.Now(),
				Payload:   json.RawMessage(line),
			})
		case "tool_result":
			safeEmit(events, provider.Event{
				Kind:      provider.EventToolResult,
				Timestamp: time.Now(),
				Payload:   json.RawMessage(line),
			})
		case "error":
			safeEmit(events, provider.Event{
				Kind:      provider.EventError,
				Timestamp: time.Now(),
				Payload:   json.RawMessage(line),
			})
		case "result", "done":
			if evt.Result != "" {
				res.FinalText = evt.Result
			}
		default:
			safeEmit(events, provider.Event{
				Kind:      provider.EventStdoutChunk,
				Timestamp: time.Now(),
				Payload:   json.RawMessage(line),
			})
		}
	}

	if res.FinalText == "" {
		res.FinalText = textBuf.String()
	}
	return res, scanner.Err()
}
