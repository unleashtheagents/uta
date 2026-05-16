package builtin

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/unleashtheagents/uta/internal/provider"
)

// Gemini is the Google Gemini CLI provider.
//
// Invocation:  gemini -p "<prompt>" --output-format stream-json [-r <id>]
//
// Output format empirically discovered at the smoke-test stage: parser is
// permissive so it handles minor schema drift across versions.
type Gemini struct{}

func (Gemini) Name() string { return "gemini" }

var geminiVersionRE = regexp.MustCompile(`(\d+\.\d+(?:\.\d+)?)`)

func (Gemini) Detect(ctx context.Context) provider.Detection {
	path, err := exec.LookPath("gemini")
	if err != nil {
		return provider.Detection{Available: false, Notes: "binary 'gemini' not found on PATH"}
	}
	out, err := exec.CommandContext(ctx, path, "--version").CombinedOutput()
	if err != nil {
		return provider.Detection{
			Available:  false,
			BinaryPath: path,
			Notes:      fmt.Sprintf("'gemini --version' failed: %v", err),
		}
	}
	version := ""
	if m := geminiVersionRE.FindString(strings.TrimSpace(string(out))); m != "" {
		version = m
	}
	notes := ""
	if strings.HasPrefix(version, "0.") {
		notes = "session-resume behavior changed across pre-1.0 versions; resume is best-effort"
	}
	return provider.Detection{
		Available:  true,
		BinaryPath: path,
		Version:    version,
		Capabilities: []provider.Capability{
			provider.CapResume,
			provider.CapStreamJSON,
			provider.CapMCP,
		},
		Notes: notes,
	}
}

func (g Gemini) RunHeadless(ctx context.Context, prompt string, opts provider.RunOptions, events chan<- provider.Event) (provider.RunResult, error) {
	return g.runHeadless(ctx, prompt, "", opts, events)
}

func (g Gemini) ResumeHeadless(ctx context.Context, sessionID, prompt string, opts provider.RunOptions, events chan<- provider.Event) (provider.RunResult, error) {
	if sessionID == "" {
		return provider.RunResult{}, fmt.Errorf("gemini resume: empty session id: %w", provider.ErrUnsupported)
	}
	return g.runHeadless(ctx, prompt, sessionID, opts, events)
}

func (Gemini) runHeadless(ctx context.Context, prompt, resumeID string, opts provider.RunOptions, events chan<- provider.Event) (provider.RunResult, error) {
	args := []string{"-p", prompt, "--output-format", "stream-json"}
	if resumeID != "" {
		args = append(args, "-r", resumeID)
	}
	args = append(args, opts.ExtraArgs...)

	if opts.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, opts.Timeout)
		defer cancel()
	}

	cmd := exec.CommandContext(ctx, "gemini", args...)
	if opts.Workdir != "" {
		cmd.Dir = opts.Workdir
	}
	if len(opts.Env) > 0 {
		cmd.Env = append(cmd.Environ(), opts.Env...)
	}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return provider.RunResult{}, fmt.Errorf("gemini stdout pipe: %w", provider.ErrTransport)
	}
	var stderrBuf bytes.Buffer
	cmd.Stderr = &stderrBuf

	if err := cmd.Start(); err != nil {
		return provider.RunResult{}, fmt.Errorf("gemini start: %w: %v", provider.ErrTransport, err)
	}

	var (
		rawBuf    bytes.Buffer
		rawMu     sync.Mutex
		sessionID string
		finalText strings.Builder
	)
	tee := newTeeWriter(&rawMu, &rawBuf)

	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 1024*1024), 16*1024*1024)
	for scanner.Scan() {
		line := scanner.Bytes()
		// Tee the raw bytes plus the newline so the blob reproduces the stream verbatim.
		_, _ = tee.Write(append(append([]byte{}, line...), '\n'))
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		sid, text := parseGeminiLine(line, events)
		if sid != "" {
			sessionID = sid
		}
		if text != "" {
			finalText.WriteString(text)
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
		FinalText: strings.TrimSpace(finalText.String()),
		RawOutput: rawBuf.Bytes(),
		ExitCode:  exit,
	}

	if waitErr != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return result, fmt.Errorf("gemini timed out: %w", provider.ErrTimeout)
		}
		if errors.Is(ctx.Err(), context.Canceled) {
			return result, ctx.Err()
		}
		stderr := strings.TrimSpace(stderrBuf.String())
		low := strings.ToLower(stderr)
		switch {
		case strings.Contains(low, "auth") || strings.Contains(low, "unauthorized") || strings.Contains(low, "api key"):
			return result, fmt.Errorf("gemini: %w: %s", provider.ErrAuth, stderr)
		case strings.Contains(low, "quota") || strings.Contains(low, "rate limit"):
			return result, fmt.Errorf("gemini: %w: %s", provider.ErrQuota, stderr)
		case exit != 0:
			return result, fmt.Errorf("gemini exit %d: %w: %s", exit, provider.ErrWorkerFailed, stderr)
		default:
			return result, fmt.Errorf("gemini: %w: %v: %s", provider.ErrTransport, waitErr, stderr)
		}
	}
	if scanErr != nil {
		return result, fmt.Errorf("gemini stream parse: %w: %v", provider.ErrTransport, scanErr)
	}
	return result, nil
}

// parseGeminiLine is permissive — it sniffs common shapes of stream-json
// events without locking us to a specific schema, since Gemini CLI's stream
// format hasn't been as well-documented as Claude's.
func parseGeminiLine(line []byte, events chan<- provider.Event) (sessionID, text string) {
	var generic map[string]any
	if err := json.Unmarshal(line, &generic); err != nil {
		safeEmit(events, provider.Event{
			Kind:      provider.EventStdoutChunk,
			Timestamp: time.Now(),
			Payload:   json.RawMessage(line),
		})
		return "", ""
	}

	// Session id may surface under a few keys.
	if v, ok := pickString(generic, "session_id", "sessionId", "session"); ok {
		payload, _ := json.Marshal(map[string]string{"session_id": v})
		safeEmit(events, provider.Event{
			Kind:      provider.EventSessionID,
			Timestamp: time.Now(),
			Payload:   payload,
		})
		sessionID = v
	}

	// Dispatch on `type`, falling through to text extraction otherwise.
	t, _ := generic["type"].(string)
	switch strings.ToLower(t) {
	case "tool_call", "tool_use":
		payload, _ := json.Marshal(generic)
		safeEmit(events, provider.Event{
			Kind:      provider.EventToolCall,
			Timestamp: time.Now(),
			Payload:   payload,
		})
	case "tool_result":
		payload, _ := json.Marshal(generic)
		safeEmit(events, provider.Event{
			Kind:      provider.EventToolResult,
			Timestamp: time.Now(),
			Payload:   payload,
		})
	case "error":
		payload, _ := json.Marshal(generic)
		safeEmit(events, provider.Event{
			Kind:      provider.EventError,
			Timestamp: time.Now(),
			Payload:   payload,
		})
	default:
		// Look for text under common shapes.
		if v, ok := pickString(generic, "text", "content", "response", "result", "output"); ok && v != "" {
			payload, _ := json.Marshal(map[string]string{"text": v})
			safeEmit(events, provider.Event{
				Kind:      provider.EventAssistantText,
				Timestamp: time.Now(),
				Payload:   payload,
			})
			text = v
		} else if msg, ok := generic["message"].(map[string]any); ok {
			if v, ok := pickString(msg, "text", "content"); ok && v != "" {
				payload, _ := json.Marshal(map[string]string{"text": v})
				safeEmit(events, provider.Event{
					Kind:      provider.EventAssistantText,
					Timestamp: time.Now(),
					Payload:   payload,
				})
				text = v
			}
		} else {
			// Unknown shape — keep as opaque chunk so nothing is lost.
			safeEmit(events, provider.Event{
				Kind:      provider.EventStdoutChunk,
				Timestamp: time.Now(),
				Payload:   json.RawMessage(line),
			})
		}
	}
	return sessionID, text
}

func pickString(m map[string]any, keys ...string) (string, bool) {
	for _, k := range keys {
		if v, ok := m[k].(string); ok && v != "" {
			return v, true
		}
	}
	return "", false
}

type teeWriter struct {
	mu  *sync.Mutex
	buf *bytes.Buffer
}

func newTeeWriter(mu *sync.Mutex, buf *bytes.Buffer) *teeWriter {
	return &teeWriter{mu: mu, buf: buf}
}

func (t *teeWriter) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.buf.Write(p)
}
