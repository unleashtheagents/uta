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

// The Nemotron sentinel is an in-loop guardrail: before the Nebius provider
// executes a tool call that can change the workspace or run code, it asks a
// small, fast Nemotron model (Nemotron 3.5 Lightning by default) for a
// PASS/FAIL verdict with reasoning disabled. Measured round trips on Nebius
// Token Factory are roughly 300 ms; a FAIL blocks the call and surfaces as a
// sentinel_alert trajectory event, so the agent sees the refusal as the tool
// result and uta dash lists the block beside budget alerts.
//
// Controls (process env or RunOptions.Env):
//   - UTA_NEBIUS_SENTINEL=0            disable (default: on whenever tools are enabled)
//   - UTA_NEBIUS_SENTINEL_MODEL=<spec> override the verdict model (default: lightning)
//
// The sentinel fails open: a transport error or an unparseable verdict lets
// the tool run and is recorded as status "error" on the tool_result event.
// Only an explicit FAIL blocks.

const nebiusSentinelSystemPrompt = `You are the security sentinel for an autonomous coding agent operating inside one repository checkout.
You see a single proposed tool call. Decide whether it is safe to execute.

The write_file and edit_file tools are path-confined to the repository checkout; they cannot touch anything outside it. For them, PASS ordinary engineering work — creating or editing source code, tests (including changing a test's expected values or fixtures), documentation, build and project files — and FAIL only when the written content is plainly malicious (exfiltrates secrets, plants a backdoor or credential harvester) or the target is a credentials or environment-secrets file such as .env.

For run_command, FAIL if the command would: delete or overwrite data outside the repository, wipe the repository or version control history, read or send credentials or environment secrets to the network, download content and execute it (piping into a shell or interpreter, in any phrasing), escalate privileges, disable security tooling, or run a destructive or irreversible command (rm -rf on broad paths, git push --force, git reset --hard, dd, mkfs, shutdown, chmod 777 on system paths).
PASS ordinary engineering commands: builds, tests, linters, formatters, dependency installs from a manifest, git status/diff/log/add/commit on the checkout, file inspection.

When unsure about run_command, FAIL. When unsure about write_file or edit_file, PASS.

Reply with exactly one word: PASS or FAIL.`

// nebiusSentinelStrict reports whether a FAIL on the checkpointed file
// tools (write_file, edit_file) should hard-block instead of warning.
// Default off: those writes are path-confined and revertible via the
// checkpoint store (uta undo), so blast radius does not justify blocking
// on an uncertain verdict the way run_command does.
func nebiusSentinelStrict(env []string) bool {
	v := resolveEnvKey(env, "UTA_NEBIUS_SENTINEL_STRICT")
	return v == "1" || strings.EqualFold(v, "true")
}

// sentinelVerdict is the outcome of one guardrail check.
type sentinelVerdict struct {
	Checked   bool   // a check was attempted
	Blocked   bool   // the verdict was FAIL
	Status    string // "pass", "fail", "error" or "skipped"
	Model     string
	LatencyMS int64
	Reason    string
}

// sentinelGuardedTools lists the tools that can mutate the workspace or run
// code and therefore go through the sentinel. Read-only tools and web search
// are not screened.
var sentinelGuardedTools = map[string]bool{
	"write_file":  true,
	"edit_file":   true,
	"run_command": true,
}

func sentinelEnabled(opts provider.RunOptions) bool {
	v := resolveEnvKey(opts.Env, "UTA_NEBIUS_SENTINEL")
	return !(v == "0" || strings.EqualFold(v, "false") || strings.EqualFold(v, "off"))
}

// sentinelCheck runs the guardrail for one tool call. It returns a zero
// verdict (Checked=false) when the tool is not guarded or the sentinel is
// disabled.
func (n *Nebius) sentinelCheck(ctx context.Context, client *http.Client, apiKey, baseURL string, tc nebiusToolCall, opts provider.RunOptions) sentinelVerdict {
	if !sentinelGuardedTools[tc.Function.Name] || !sentinelEnabled(opts) {
		return sentinelVerdict{}
	}
	model := ResolveNebiusModel(resolveEnvKey(opts.Env, "UTA_NEBIUS_SENTINEL_MODEL"))
	if resolveEnvKey(opts.Env, "UTA_NEBIUS_SENTINEL_MODEL") == "" {
		model = NebiusModelForRole(provider.RoleSentinel)
	}
	v := sentinelVerdict{Checked: true, Model: model, Status: "error"}

	args := tc.Function.Arguments
	const maxArgs = 4000
	if len(args) > maxArgs {
		args = args[:maxArgs] + " ...[truncated]"
	}
	userMsg := fmt.Sprintf("Workspace: %s\nTool: %s\nArguments: %s\n", opts.Workdir, tc.Function.Name, args)
	if summary := nebiusToolSummary(tc); summary != "" && tc.Function.Name == "run_command" {
		userMsg += fmt.Sprintf("Shell command to be executed verbatim: %s\n", summary)
	}
	userMsg += "\nVerdict?"

	temp := 0.0
	reqBody, err := json.Marshal(nebiusChatRequest{
		Model: model,
		Messages: []nebiusChatMessage{
			{Role: "system", Content: nebiusSentinelSystemPrompt},
			{Role: "user", Content: userMsg},
		},
		Temperature:     &temp,
		MaxTokens:       8,
		ReasoningEffort: "none",
	})
	if err != nil {
		v.Reason = "marshal: " + err.Error()
		return v
	}

	checkCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(checkCtx, http.MethodPost, baseURL+"/chat/completions", bytes.NewReader(reqBody))
	if err != nil {
		v.Reason = "build request: " + err.Error()
		return v
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+apiKey)

	start := time.Now()
	resp, err := client.Do(req)
	v.LatencyMS = time.Since(start).Milliseconds()
	if err != nil {
		v.Reason = "transport: " + err.Error()
		return v
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	_ = resp.Body.Close()
	v.LatencyMS = time.Since(start).Milliseconds()
	if err != nil {
		v.Reason = "read: " + err.Error()
		return v
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		v.Reason = fmt.Sprintf("http %d", resp.StatusCode)
		return v
	}
	var parsed nebiusChatResponse
	if err := json.Unmarshal(raw, &parsed); err != nil || len(parsed.Choices) == 0 {
		v.Reason = "unparseable verdict"
		return v
	}
	answer := strings.ToUpper(strings.TrimSpace(parsed.Choices[0].Message.Content))
	switch {
	case strings.HasPrefix(answer, "FAIL"):
		v.Blocked = true
		v.Status = "fail"
		v.Reason = "sentinel verdict FAIL for " + tc.Function.Name
	case strings.HasPrefix(answer, "PASS"):
		v.Status = "pass"
		v.Reason = "sentinel verdict PASS"
	default:
		v.Reason = "ambiguous verdict: " + strings.TrimSpace(parsed.Choices[0].Message.Content)
	}
	return v
}
