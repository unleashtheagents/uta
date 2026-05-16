// Package declarative implements an AgentProvider that's driven entirely by
// a YAML descriptor (see internal/config.ProviderDescriptor). This is how
// users add a new agent CLI to uta without writing Go code.
package declarative

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

	"github.com/unleashtheagents/uta/internal/config"
	"github.com/unleashtheagents/uta/internal/provider"
)

// Provider is a YAML-driven AgentProvider.
type Provider struct {
	desc       *config.ProviderDescriptor
	versionRE  *regexp.Regexp
	capability map[provider.Capability]bool
}

// New builds a runtime provider from a parsed descriptor.
func New(desc *config.ProviderDescriptor) (*Provider, error) {
	re, err := regexp.Compile(desc.Detect.VersionRegex)
	if err != nil {
		return nil, fmt.Errorf("compile version regex: %w", err)
	}
	caps := map[provider.Capability]bool{}
	for _, c := range desc.Capabilities {
		caps[provider.Capability(c)] = true
	}
	// resume_argv presence implies CapResume even if not listed.
	if len(desc.Invocation.ResumeArgv) > 0 {
		caps[provider.CapResume] = true
	}
	if desc.Output.Format == "stream-json" {
		caps[provider.CapStreamJSON] = true
	}
	return &Provider{desc: desc, versionRE: re, capability: caps}, nil
}

func (p *Provider) Name() string { return p.desc.Name }

func (p *Provider) Detect(ctx context.Context) provider.Detection {
	binPath, err := exec.LookPath(p.desc.Binary)
	if err != nil {
		return provider.Detection{Available: false, Notes: fmt.Sprintf("binary %q not found on PATH", p.desc.Binary)}
	}
	cmd := exec.CommandContext(ctx, binPath, p.desc.Detect.Args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return provider.Detection{
			Available:  false,
			BinaryPath: binPath,
			Notes:      fmt.Sprintf("'%s %s' failed: %v", p.desc.Binary, strings.Join(p.desc.Detect.Args, " "), err),
		}
	}
	version := ""
	if m := p.versionRE.FindStringSubmatch(string(out)); len(m) > 1 {
		version = m[1]
	} else if m := p.versionRE.FindString(string(out)); m != "" {
		version = m
	}
	caps := make([]provider.Capability, 0, len(p.capability))
	for c := range p.capability {
		caps = append(caps, c)
	}
	return provider.Detection{
		Available:    true,
		BinaryPath:   binPath,
		Version:      version,
		Capabilities: caps,
		Notes:        p.desc.Notes,
	}
}

func (p *Provider) RunHeadless(ctx context.Context, prompt string, opts provider.RunOptions, events chan<- provider.Event) (provider.RunResult, error) {
	return p.run(ctx, prompt, "", opts, events)
}

func (p *Provider) ResumeHeadless(ctx context.Context, sessionID, prompt string, opts provider.RunOptions, events chan<- provider.Event) (provider.RunResult, error) {
	if !p.capability[provider.CapResume] || len(p.desc.Invocation.ResumeArgv) == 0 {
		return provider.RunResult{}, fmt.Errorf("declarative provider %q: %w", p.desc.Name, provider.ErrUnsupported)
	}
	return p.run(ctx, prompt, sessionID, opts, events)
}

func (p *Provider) run(ctx context.Context, prompt, resumeID string, opts provider.RunOptions, events chan<- provider.Event) (provider.RunResult, error) {
	vars := map[string]string{
		"prompt":        prompt,
		"session_id":    resumeID,
		"output_format": p.desc.Output.Format,
		"workdir":       opts.Workdir,
	}
	argv := p.desc.Invocation.Argv
	if resumeID != "" {
		argv = p.desc.Invocation.ResumeArgv
	}
	rendered := make([]string, 0, len(argv))
	for _, a := range argv {
		rendered = append(rendered, substitute(a, vars))
	}
	rendered = append(rendered, opts.ExtraArgs...)

	if opts.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, opts.Timeout)
		defer cancel()
	}

	cmd := exec.CommandContext(ctx, p.desc.Binary, rendered...)
	if opts.Workdir != "" {
		cmd.Dir = opts.Workdir
	}
	env := cmd.Environ()
	for k, v := range p.desc.Invocation.Env {
		env = append(env, k+"="+v)
	}
	env = append(env, opts.Env...)
	cmd.Env = env

	var stdinPiped bool
	if p.desc.Invocation.Stdin != "" {
		stdin, err := cmd.StdinPipe()
		if err != nil {
			return provider.RunResult{}, fmt.Errorf("stdin pipe: %w", provider.ErrTransport)
		}
		stdinPiped = true
		go func() {
			_, _ = io.WriteString(stdin, substitute(p.desc.Invocation.Stdin, vars))
			_ = stdin.Close()
		}()
	}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return provider.RunResult{}, fmt.Errorf("stdout pipe: %w", provider.ErrTransport)
	}
	var stderrBuf bytes.Buffer
	cmd.Stderr = &stderrBuf

	if err := cmd.Start(); err != nil {
		return provider.RunResult{}, fmt.Errorf("start: %w: %v", provider.ErrTransport, err)
	}

	var (
		rawBuf    bytes.Buffer
		rawMu     sync.Mutex
		sessionID string
		finalText strings.Builder
	)

	switch p.desc.Output.Format {
	case "stream-json":
		sid, ft := p.parseStreamJSON(stdout, &rawBuf, &rawMu, events)
		sessionID = sid
		finalText.WriteString(ft)
	case "text":
		all, _ := io.ReadAll(stdout)
		rawMu.Lock()
		rawBuf.Write(all)
		rawMu.Unlock()
		finalText.Write(all)
	}

	waitErr := cmd.Wait()
	exit := 0
	if exitErr, ok := waitErr.(*exec.ExitError); ok {
		exit = exitErr.ExitCode()
	}

	_ = stdinPiped

	result := provider.RunResult{
		SessionID: sessionID,
		FinalText: strings.TrimSpace(finalText.String()),
		RawOutput: rawBuf.Bytes(),
		ExitCode:  exit,
	}

	if waitErr != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return result, fmt.Errorf("%s timed out: %w", p.desc.Name, provider.ErrTimeout)
		}
		if errors.Is(ctx.Err(), context.Canceled) {
			return result, ctx.Err()
		}
		stderr := strings.TrimSpace(stderrBuf.String())
		low := strings.ToLower(stderr)
		switch {
		case strings.Contains(low, "auth") || strings.Contains(low, "unauthorized") || strings.Contains(low, "api key"):
			return result, fmt.Errorf("%s: %w: %s", p.desc.Name, provider.ErrAuth, stderr)
		case strings.Contains(low, "quota") || strings.Contains(low, "rate limit"):
			return result, fmt.Errorf("%s: %w: %s", p.desc.Name, provider.ErrQuota, stderr)
		case exit != 0:
			return result, fmt.Errorf("%s exit %d: %w: %s", p.desc.Name, exit, provider.ErrWorkerFailed, stderr)
		default:
			return result, fmt.Errorf("%s: %w: %v: %s", p.desc.Name, provider.ErrTransport, waitErr, stderr)
		}
	}
	return result, nil
}

func (p *Provider) parseStreamJSON(r io.Reader, rawBuf *bytes.Buffer, rawMu *sync.Mutex, events chan<- provider.Event) (sessionID, finalText string) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 1024*1024), 16*1024*1024)
	for scanner.Scan() {
		line := scanner.Bytes()
		// tee
		rawMu.Lock()
		rawBuf.Write(line)
		rawBuf.WriteByte('\n')
		rawMu.Unlock()
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var generic map[string]any
		if err := json.Unmarshal(line, &generic); err != nil {
			emit(events, provider.EventStdoutChunk, json.RawMessage(line))
			continue
		}
		// session_id
		if p.desc.Output.SessionIDField != "" {
			if v, ok := generic[p.desc.Output.SessionIDField].(string); ok && v != "" {
				sessionID = v
				payload, _ := json.Marshal(map[string]string{"session_id": v})
				emit(events, provider.EventSessionID, payload)
			}
		}
		// final text
		if p.desc.Output.FinalTextField != "" {
			if v, ok := generic[p.desc.Output.FinalTextField].(string); ok && v != "" {
				finalText = v
			}
		}
		// event dispatch by `type`
		if t, ok := generic["type"].(string); ok {
			if kind, ok := p.desc.Output.EventDispatch[t]; ok {
				payload, _ := json.Marshal(generic)
				emit(events, mapEventKind(kind), payload)
				continue
			}
		}
		// fall back: look for text in common shapes
		if v, ok := generic[p.desc.Output.TextField].(string); ok && v != "" {
			payload, _ := json.Marshal(map[string]string{"text": v})
			emit(events, provider.EventAssistantText, payload)
		}
	}
	return sessionID, finalText
}

func mapEventKind(s string) provider.EventKind {
	switch s {
	case "assistant_text":
		return provider.EventAssistantText
	case "tool_call":
		return provider.EventToolCall
	case "tool_result":
		return provider.EventToolResult
	case "error":
		return provider.EventError
	default:
		return provider.EventStdoutChunk
	}
}

func emit(events chan<- provider.Event, kind provider.EventKind, payload json.RawMessage) {
	if events == nil {
		return
	}
	select {
	case events <- provider.Event{Kind: kind, Timestamp: time.Now(), Payload: payload}:
	default:
	}
}

func substitute(s string, vars map[string]string) string {
	out := s
	for k, v := range vars {
		out = strings.ReplaceAll(out, "{{"+k+"}}", v)
	}
	return out
}
