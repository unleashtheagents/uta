package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// ToolSpec configures one external analysis tool that runs alongside the
// critics during a Reflector iteration. The tool's stdout is parsed by an
// adapter into Findings that join the same FindingsReport.
type ToolSpec struct {
	// ID is the stable identifier of this tool's findings (used as the
	// `critic` field on the resulting Finding entries).
	ID string

	// Cmd is the executable name OR a shell command (when ShellMode is true).
	// For built-in adapters with no explicit Cmd, the registry resolves
	// the default invocation.
	Cmd string

	// Args are passed to Cmd (when ShellMode is false).
	Args []string

	// ShellMode runs Cmd through `sh -c` instead of exec.Command. Useful
	// for piping (e.g. "slither . | tee log.json").
	ShellMode bool

	// Adapter selects the stdout parser. "" = "raw" (no parsing).
	// Built-ins: "slither", "mythril", "forge-test", "raw".
	Adapter string

	// Workdir overrides the default workdir.
	Workdir string

	// Timeout overrides the default per-tool timeout.
	Timeout time.Duration

	// ContinueOnFailure controls whether a non-zero exit from the tool is
	// treated as the tool reporting findings (true, like slither/mythril
	// which exit nonzero when issues are present) or as the tool itself
	// failing (false).
	ContinueOnFailure bool

	// Env supplies per-tool environment variables, layered on top of the
	// parent process environment. Entries here override matching keys from
	// os.Environ(). Use this to pass API keys or scanner config to a single
	// tool without leaking them into every other tool or critic.
	Env map[string]string
}

// ToolResult is what runTool returns.
type ToolResult struct {
	ID       string
	ExitCode int
	Stdout   string
	Stderr   string
	Findings []Finding
	Duration time.Duration
	Err      error // wraps spawn/parse errors. Set to nil if the tool ran AND parsed AND ContinueOnFailure==true OR exit==0.
}

// BuiltinAdapter resolves an adapter by name into a parser function that
// takes raw stdout (and the tool ID for finding attribution) and returns
// findings.
type BuiltinAdapter func(toolID string, stdout, stderr string) ([]Finding, error)

var builtinAdapters = map[string]BuiltinAdapter{
	"raw":        parseRaw,
	"findings":   parseFindingsAdapter,
	"slither":    parseSlither,
	"mythril":    parseMythril,
	"forge-test": parseForgeTest,
	"aderyn":     parseAderyn,
}

// ResolveToolSpec fills defaults for known IDs. If a user passes `--tool slither`
// with no other config, this resolves to the canonical invocation + slither
// adapter. Returns a copy.
func ResolveToolSpec(in ToolSpec) ToolSpec {
	out := in
	if out.ID == "" && out.Cmd != "" {
		// derive an ID from the binary name
		fields := strings.Fields(out.Cmd)
		if len(fields) > 0 {
			out.ID = fields[0]
		}
	}
	switch strings.ToLower(out.ID) {
	case "slither":
		if out.Cmd == "" {
			out.Cmd = "slither"
			out.Args = append([]string{".", "--json", "-"}, out.Args...)
		}
		if out.Adapter == "" {
			out.Adapter = "slither"
		}
		out.ContinueOnFailure = true // slither exits 255 when it finds issues
	case "mythril":
		if out.Cmd == "" {
			out.Cmd = "myth"
			out.Args = append([]string{"analyze", "-o", "json"}, out.Args...)
		}
		if out.Adapter == "" {
			out.Adapter = "mythril"
		}
		out.ContinueOnFailure = true
	case "forge-test", "forge":
		if out.Cmd == "" {
			out.Cmd = "forge"
			out.Args = append([]string{"test", "--json"}, out.Args...)
		}
		if out.Adapter == "" {
			out.Adapter = "forge-test"
		}
		// forge test exits non-zero when tests fail — those are our findings.
		out.ContinueOnFailure = true
	case "aderyn":
		if out.Cmd == "" {
			out.Cmd = "aderyn"
			// `aderyn . --output -` writes the JSON report to stdout instead
			// of the default ./report.json on disk.
			out.Args = append([]string{".", "--output", "-"}, out.Args...)
		}
		if out.Adapter == "" {
			out.Adapter = "aderyn"
		}
		// aderyn exits 0 even when it finds issues; non-zero means the
		// binary itself failed, so we keep ContinueOnFailure false.
	}
	if out.Adapter == "" {
		out.Adapter = "raw"
	}
	if out.Timeout == 0 {
		out.Timeout = 10 * time.Minute
	}
	return out
}

// runTool executes the tool and parses its stdout via the configured
// adapter. Errors are categorized: spawn errors return Err set;
// non-zero exit with ContinueOnFailure=true is NOT an error (tool reported
// findings); parse errors return Err set with the raw output preserved.
func runTool(parent context.Context, spec ToolSpec) ToolResult {
	res := ToolResult{ID: spec.ID}
	if spec.Cmd == "" {
		res.Err = fmt.Errorf("tool %q: cmd is empty", spec.ID)
		return res
	}

	ctx := parent
	var cancel context.CancelFunc
	if spec.Timeout > 0 {
		ctx, cancel = context.WithTimeout(parent, spec.Timeout)
		defer cancel()
	}

	start := time.Now()
	var cmd *exec.Cmd
	if spec.ShellMode {
		shellCmd := spec.Cmd
		if len(spec.Args) > 0 {
			shellCmd = shellCmd + " " + strings.Join(spec.Args, " ")
		}
		cmd = exec.CommandContext(ctx, "sh", "-c", shellCmd)
	} else {
		cmd = exec.CommandContext(ctx, spec.Cmd, spec.Args...)
	}
	if spec.Workdir != "" {
		cmd.Dir = spec.Workdir
	}
	if len(spec.Env) > 0 {
		cmd.Env = mergeEnv(os.Environ(), spec.Env)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	runErr := runCommand(cmd)
	res.Stdout = stdout.String()
	res.Stderr = stderr.String()
	res.Duration = time.Since(start)

	if runErr != nil {
		var ee *exec.ExitError
		if errors.As(runErr, &ee) {
			res.ExitCode = ee.ExitCode()
			if !spec.ContinueOnFailure {
				res.Err = fmt.Errorf("tool %q exit %d", spec.ID, res.ExitCode)
				return res
			}
			// fall through to parse — the tool emitted findings via non-zero
		} else {
			// spawn failure: binary not on PATH, etc.
			res.Err = fmt.Errorf("tool %q: %w", spec.ID, runErr)
			return res
		}
	}

	adapter, ok := builtinAdapters[spec.Adapter]
	if !ok {
		adapter = builtinAdapters["raw"]
	}
	findings, perr := adapter(spec.ID, res.Stdout, res.Stderr)
	if perr != nil {
		res.Err = fmt.Errorf("tool %q parse: %w", spec.ID, perr)
		return res
	}
	res.Findings = findings
	return res
}

// ---------- adapters ----------

func parseRaw(toolID, stdout, stderr string) ([]Finding, error) {
	// No findings extracted, but record nothing-is-error.
	return nil, nil
}

// parseFindingsAdapter delegates to the same permissive ParseFindings used
// for critic outputs. Used as the default for ad-hoc shell tools that emit
// JSON; failed parses degrade to empty findings rather than erroring (the
// tool may legitimately have nothing to say).
func parseFindingsAdapter(toolID, stdout, _ string) ([]Finding, error) {
	findings, err := ParseFindings(stdout)
	if err != nil {
		return nil, nil
	}
	for i := range findings {
		if findings[i].ID == "" {
			findings[i].ID = fmt.Sprintf("%s-%d", toolID, i)
		}
	}
	return findings, nil
}

// parseMythril decodes mythril's --o json output. Two known schemas exist
// across versions; we handle the common ones permissively.
//
//	{"issues":[{"swc-id":"...","severity":"...","title":"...","description":"...","filename":"...","lineno":N}]}
//	{"messages":[...],"issues":[...]} (newer)
func parseMythril(toolID, stdout, _ string) ([]Finding, error) {
	payload := extractJSONObject(stdout)
	if payload == "" {
		return nil, nil
	}
	var raw struct {
		Issues []struct {
			SWC         string `json:"swc-id"`
			Severity    string `json:"severity"`
			Title       string `json:"title"`
			Description string `json:"description"`
			Filename    string `json:"filename"`
			Lineno      int    `json:"lineno"`
			SourceMap   string `json:"sourceMap"`
		} `json:"issues"`
	}
	if err := json.Unmarshal([]byte(payload), &raw); err != nil {
		return nil, fmt.Errorf("mythril json: %w", err)
	}
	out := make([]Finding, 0, len(raw.Issues))
	for i, is := range raw.Issues {
		id := is.SWC
		if id == "" {
			id = fmt.Sprintf("mythril-%d", i)
		}
		out = append(out, Finding{
			ID:       id,
			Severity: NormalizeSeverity(is.Severity),
			Title:    is.Title,
			Body:     strings.TrimSpace(is.Description),
			File:     is.Filename,
			Line:     is.Lineno,
		})
	}
	return out, nil
}

// parseForgeTest decodes `forge test --json` output. Each test contract is a
// top-level key whose value contains `test_results`. We treat each FAILED
// test as a HIGH finding (a failing test is by definition an exploitable
// invariant violation).
func parseForgeTest(toolID, stdout, _ string) ([]Finding, error) {
	stdout = strings.TrimSpace(stdout)
	if stdout == "" {
		return nil, nil
	}
	// forge test --json emits a top-level map: contract_path -> {test_results: {name: {status, reason, ...}}}
	var root map[string]struct {
		TestResults map[string]struct {
			Status string `json:"status"`
			Reason string `json:"reason"`
		} `json:"test_results"`
	}
	if err := json.Unmarshal([]byte(stdout), &root); err != nil {
		return nil, fmt.Errorf("forge-test json: %w", err)
	}
	var out []Finding
	idx := 0
	for contract, tr := range root {
		for name, r := range tr.TestResults {
			if strings.EqualFold(r.Status, "success") {
				continue
			}
			idx++
			body := r.Reason
			if body == "" {
				body = "test failed: status=" + r.Status
			}
			base := filepath.Base(contract)
			out = append(out, Finding{
				ID:       fmt.Sprintf("forge-%d", idx),
				Severity: SevHigh, // a failing invariant test is HIGH
				Title:    fmt.Sprintf("forge: %s::%s failed", base, name),
				Body:     body,
				File:     base,
			})
		}
	}
	return out, nil
}

// KnownBuiltinTools lists adapters baked into the binary, for `uta doctor`.
func KnownBuiltinTools() []string {
	return []string{"slither", "mythril", "forge-test", "aderyn"}
}

// mergeEnv layers overrides on top of a base "K=V" slice. Keys in overrides
// replace matching entries from base; the remaining base entries are kept in
// their original order. Overrides are appended in sorted key order so the
// result is deterministic across runs.
func mergeEnv(base []string, overrides map[string]string) []string {
	if len(overrides) == 0 {
		return base
	}
	out := make([]string, 0, len(base)+len(overrides))
	for _, kv := range base {
		eq := strings.IndexByte(kv, '=')
		if eq <= 0 {
			out = append(out, kv)
			continue
		}
		if _, replace := overrides[kv[:eq]]; replace {
			continue
		}
		out = append(out, kv)
	}
	keys := make([]string, 0, len(overrides))
	for k := range overrides {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		out = append(out, k+"="+overrides[k])
	}
	return out
}
