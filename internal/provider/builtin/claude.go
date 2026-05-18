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

// Claude is the Claude Code provider (the `claude` CLI from Anthropic).
//
// Invocation:  claude -p --output-format stream-json --verbose [--resume <id>]
//              [--allowedTools ...] [--add-dir <workdir>]
// Prompt is fed via stdin (avoids argv length and quoting bugs).
type Claude struct {
	pathOnce sync.Once
	path     string
	pathErr  error
}

func (c *Claude) Name() string { return "claude" }

// resolvePath looks up the absolute path to `claude` exactly once and caches
// the result. We resolve up front (not per RunHeadless) so a PATH change in a
// subtask environment can't redirect us to a different binary mid-run.
func (c *Claude) resolvePath() (string, error) {
	c.pathOnce.Do(func() {
		c.path, c.pathErr = exec.LookPath("claude")
	})
	return c.path, c.pathErr
}

var claudeVersionRE = regexp.MustCompile(`(\d+\.\d+(?:\.\d+)?)`)

func (c *Claude) Detect(ctx context.Context) provider.Detection {
	path, err := c.resolvePath()
	if err != nil {
		return provider.Detection{Available: false, Notes: "binary 'claude' not found on PATH"}
	}
	out, err := exec.CommandContext(ctx, path, "--version").CombinedOutput()
	if err != nil {
		return provider.Detection{
			Available:  false,
			BinaryPath: path,
			Notes:      fmt.Sprintf("'claude --version' failed: %v", err),
		}
	}
	version := ""
	if m := claudeVersionRE.FindString(strings.TrimSpace(string(out))); m != "" {
		version = m
	}
	return provider.Detection{
		Available:  true,
		BinaryPath: path,
		Version:    version,
		Capabilities: []provider.Capability{
			provider.CapResume,
			provider.CapStreamJSON,
			provider.CapToolPreApprove,
			provider.CapMCP,
		},
	}
}

func (c *Claude) RunHeadless(ctx context.Context, prompt string, opts provider.RunOptions, events chan<- provider.Event) (provider.RunResult, error) {
	return c.runHeadless(ctx, prompt, "", opts, events)
}

func (c *Claude) ResumeHeadless(ctx context.Context, sessionID, prompt string, opts provider.RunOptions, events chan<- provider.Event) (provider.RunResult, error) {
	if sessionID == "" {
		return provider.RunResult{}, fmt.Errorf("claude resume: empty session id: %w", provider.ErrUnsupported)
	}
	return c.runHeadless(ctx, prompt, sessionID, opts, events)
}

func (c *Claude) runHeadless(ctx context.Context, prompt, resumeID string, opts provider.RunOptions, events chan<- provider.Event) (provider.RunResult, error) {
	args := []string{"-p", "--output-format", "stream-json", "--verbose"}
	if resumeID != "" {
		args = append(args, "--resume", resumeID)
	}
	if len(opts.PreApproveTools) > 0 {
		args = append(args, "--allowedTools", strings.Join(opts.PreApproveTools, ","))
	}
	if opts.Workdir != "" {
		args = append(args, "--add-dir", opts.Workdir)
	}
	args = append(args, opts.ExtraArgs...)

	if opts.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, opts.Timeout)
		defer cancel()
	}

	binPath, err := c.resolvePath()
	if err != nil {
		return provider.RunResult{}, fmt.Errorf("claude: %w: binary 'claude' not found on PATH", provider.ErrTransport)
	}
	cmd := exec.CommandContext(ctx, binPath, args...)
	if opts.Workdir != "" {
		cmd.Dir = opts.Workdir
	}
	if len(opts.Env) > 0 {
		cmd.Env = append(cmd.Environ(), opts.Env...)
	}

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return provider.RunResult{}, fmt.Errorf("claude stdin pipe: %w", provider.ErrTransport)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return provider.RunResult{}, fmt.Errorf("claude stdout pipe: %w", provider.ErrTransport)
	}
	var stderrBuf bytes.Buffer
	cmd.Stderr = &stderrBuf

	if err := cmd.Start(); err != nil {
		return provider.RunResult{}, fmt.Errorf("claude start: %w: %v", provider.ErrTransport, err)
	}

	// Feed prompt and close stdin so claude knows we're done.
	go func() {
		_, _ = io.WriteString(stdin, prompt)
		_ = stdin.Close()
	}()

	var (
		rawBuf      bytes.Buffer
		rawMu       sync.Mutex
		sessionID   string
		finalText   string
	)
	tee := io.TeeReader(stdout, &lockedWriter{mu: &rawMu, w: &rawBuf})

	scanner := bufio.NewScanner(tee)
	scanner.Buffer(make([]byte, 1024*1024), 16*1024*1024) // generous max-line for large tool outputs
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		sid, finalChunk := parseClaudeLine(line, events)
		if sid != "" {
			sessionID = sid
		}
		if finalChunk != "" {
			finalText = finalChunk
		}
	}
	scanErr := scanner.Err()

	waitErr := cmd.Wait()
	exit := 0
	if exitErr, ok := waitErr.(*exec.ExitError); ok {
		exit = exitErr.ExitCode()
	}

	result := provider.RunResult{
		SessionID: sessionID,
		FinalText: finalText,
		RawOutput: rawBuf.Bytes(),
		ExitCode:  exit,
	}

	if waitErr != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return result, fmt.Errorf("claude timed out: %w", provider.ErrTimeout)
		}
		if errors.Is(ctx.Err(), context.Canceled) {
			return result, ctx.Err()
		}
		stderr := strings.TrimSpace(stderrBuf.String())
		low := strings.ToLower(stderr)
		// Order matters: quota / rate-limit signals come BEFORE auth signals
		// so a 429 response that happens to contain "Authorization:" header
		// text isn't misclassified as an auth failure.
		switch {
		case strings.Contains(low, "quota") ||
			strings.Contains(low, "rate limit") ||
			strings.Contains(low, "ratelimit") ||
			strings.Contains(low, "rate_limit") ||
			strings.Contains(low, "too many requests") ||
			strings.Contains(low, "overloaded") ||
			strings.Contains(low, "status 429"):
			return result, fmt.Errorf("claude: %w: %s", provider.ErrQuota, stderr)
		case strings.Contains(low, "invalid api key") ||
			strings.Contains(low, "invalid_api_key") ||
			strings.Contains(low, "unauthorized") ||
			strings.Contains(low, "authentication failed") ||
			strings.Contains(low, "api key not valid"):
			return result, fmt.Errorf("claude: %w: %s", provider.ErrAuth, stderr)
		case exit != 0:
			return result, fmt.Errorf("claude exit %d: %w: %s", exit, provider.ErrWorkerFailed, stderr)
		default:
			return result, fmt.Errorf("claude: %w: %v: %s", provider.ErrTransport, waitErr, stderr)
		}
	}
	if scanErr != nil {
		return result, fmt.Errorf("claude stream parse: %w: %v", provider.ErrTransport, scanErr)
	}
	return result, nil
}

// parseClaudeLine decodes one stream-json line and emits the corresponding
// uta events. It returns (sessionID, finalText) extracted from system-init
// and result envelopes.
func parseClaudeLine(line []byte, events chan<- provider.Event) (string, string) {
	var env struct {
		Type    string          `json:"type"`
		Subtype string          `json:"subtype,omitempty"`
		Session string          `json:"session_id,omitempty"`
		Result  string          `json:"result,omitempty"`
		Message json.RawMessage `json:"message,omitempty"`
	}
	if err := json.Unmarshal(line, &env); err != nil {
		// Not parseable — emit as opaque stdout chunk.
		safeEmit(events, provider.Event{
			Kind:      provider.EventStdoutChunk,
			Timestamp: time.Now(),
			Payload:   json.RawMessage(line),
		})
		return "", ""
	}
	switch env.Type {
	case "system":
		if env.Subtype == "init" && env.Session != "" {
			payload, _ := json.Marshal(map[string]string{"session_id": env.Session})
			safeEmit(events, provider.Event{
				Kind:      provider.EventSessionID,
				Timestamp: time.Now(),
				Payload:   payload,
			})
			return env.Session, ""
		}
	case "assistant":
		emitClaudeMessage(env.Message, events, false)
	case "user":
		emitClaudeMessage(env.Message, events, true)
	case "result":
		if env.Session != "" || env.Result != "" {
			return env.Session, env.Result
		}
	}
	return "", ""
}

func emitClaudeMessage(msg json.RawMessage, events chan<- provider.Event, isUser bool) {
	if len(msg) == 0 {
		return
	}
	var m struct {
		Content []json.RawMessage `json:"content"`
	}
	if err := json.Unmarshal(msg, &m); err != nil {
		return
	}
	for _, raw := range m.Content {
		var block struct {
			Type      string          `json:"type"`
			Text      string          `json:"text,omitempty"`
			Name      string          `json:"name,omitempty"`
			Input     json.RawMessage `json:"input,omitempty"`
			ToolUseID string          `json:"tool_use_id,omitempty"`
			Content   json.RawMessage `json:"content,omitempty"`
		}
		if err := json.Unmarshal(raw, &block); err != nil {
			continue
		}
		switch block.Type {
		case "text":
			if isUser || block.Text == "" {
				continue
			}
			payload, _ := json.Marshal(map[string]string{"text": block.Text})
			safeEmit(events, provider.Event{
				Kind:      provider.EventAssistantText,
				Timestamp: time.Now(),
				Payload:   payload,
			})
		case "tool_use":
			payload, _ := json.Marshal(map[string]any{"name": block.Name, "input": block.Input})
			safeEmit(events, provider.Event{
				Kind:      provider.EventToolCall,
				Timestamp: time.Now(),
				Payload:   payload,
			})
		case "tool_result":
			payload, _ := json.Marshal(map[string]any{"tool_use_id": block.ToolUseID, "content": block.Content})
			safeEmit(events, provider.Event{
				Kind:      provider.EventToolResult,
				Timestamp: time.Now(),
				Payload:   payload,
			})
		}
	}
}

