package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/unleashtheagents/uta/internal/engine"
	"github.com/unleashtheagents/uta/internal/improve"
	"github.com/unleashtheagents/uta/internal/mcp"
	"github.com/unleashtheagents/uta/internal/memory"
	"github.com/unleashtheagents/uta/internal/redact"
	"github.com/unleashtheagents/uta/internal/trajectory"
	"github.com/unleashtheagents/uta/internal/version"
	"github.com/unleashtheagents/uta/internal/whiteboard"
)

func newServeCmd() *cobra.Command {
	var (
		mcpMode     bool
		printConfig string
	)
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "expose uta as a server (MCP over stdio)",
		Long: `uta serve --mcp speaks JSON-RPC 2.0 over stdin/stdout per the Model
Context Protocol spec. Any MCP-aware client (Claude Code, IDEs, other
agents) can launch this process and call into uta's tools.

For client setup, run "uta serve --print-mcp-config <client>" where
<client> is one of: claude-code, cursor, generic. The emitted JSON is
ready to paste into the client's MCP config file (typically .mcp.json
for Claude Code, ~/.cursor/mcp.json for Cursor). The full guide lives
at docs/mcp-server.md.

Tools advertised:
  uta_providers_list   detected agent providers + capabilities
  uta_ideas_list       per-project backlog
  uta_ideas_add        push a new idea
  uta_ideas_show       fetch one idea by id
  uta_sessions_list    recent orchestration runs
  uta_trajectory_get   timeline of one session
  uta_audit            run a Reflector audit on a directory (one iteration,
                       audit-only — no auto-fix from MCP for safety)
  uta_run              one-shot goal execution
  uta_improve_pick     pick the next-priority idea (read-only)
  uta_whiteboard_set   post a note to the inter-agent whiteboard
  uta_whiteboard_get   read the latest note for one key
  uta_whiteboard_list  list the latest note per distinct key

Project-aware: if uta is launched inside a project tree, ideas / sessions
/ trajectories are project-scoped. Otherwise they're global.

Generic by design: every uta capability that has a CLI surface should
eventually have an MCP tool surface. This release ships the read-heavy
subset plus audit + run (which are the high-value write operations).
Mutation surface stays narrow on purpose — the MCP client model is one
of cooperation, not blanket trust.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if printConfig != "" {
				return runPrintMCPConfig(cmd, printConfig)
			}
			if !mcpMode {
				return errors.New("pass --mcp to serve, or --print-mcp-config <client> for a config snippet")
			}
			app, err := newApp(cmd.Context())
			if err != nil {
				return err
			}
			defer app.Close()

			server := mcp.NewServer("uta", version.Version)
			registerMCPTools(server, app)

			fmt.Fprintf(cmd.ErrOrStderr(), "uta MCP server ready · proto=%s · tools=%d · project=%s\n",
				mcp.ProtocolVersion, server.ToolCount(), mcpProjectLabel(app))

			ctx, cancel := signalContext(cmd.Context())
			defer cancel()
			return server.Serve(ctx, cmd.InOrStdin(), cmd.OutOrStdout())
		},
	}
	cmd.Flags().BoolVar(&mcpMode, "mcp", false, "speak MCP (JSON-RPC 2.0) on stdin/stdout")
	cmd.Flags().StringVar(&printConfig, "print-mcp-config", "", "print an MCP client config snippet (claude-code|cursor|generic) and exit")
	return cmd
}

// runPrintMCPConfig writes a ready-to-paste MCP client configuration to
// stdout. The shape is identical to the files under examples/mcp/ so a
// user can `uta serve --print-mcp-config claude-code > .mcp.json` and be
// done. Returns a clear error on unknown clients.
func runPrintMCPConfig(cmd *cobra.Command, client string) error {
	snippet, ok := mcpConfigSnippet(client)
	if !ok {
		return fmt.Errorf("unknown MCP client %q (want one of: claude-code, cursor, generic)", client)
	}
	fmt.Fprintln(cmd.OutOrStdout(), snippet)
	return nil
}

// mcpConfigSnippet returns the JSON body matching examples/mcp/<client>.mcp.json.
// Kept inline (rather than embedded) so the binary stays self-contained and
// the examples directory is the canonical, human-editable reference.
func mcpConfigSnippet(client string) (string, bool) {
	switch client {
	case "claude-code", "claude":
		return `{
  "mcpServers": {
    "uta": {
      "command": "uta",
      "args": ["serve", "--mcp"]
    }
  }
}`, true
	case "cursor":
		return `{
  "mcpServers": {
    "uta": {
      "command": "uta",
      "args": ["serve", "--mcp"]
    }
  }
}`, true
	case "generic":
		return `{
  "name": "uta",
  "transport": {
    "type": "stdio",
    "command": "uta",
    "args": ["serve", "--mcp"]
  }
}`, true
	}
	return "", false
}

func mcpProjectLabel(app *App) string {
	if app.InProject() {
		return app.ProjectName + " (" + app.ProjectRoot + ")"
	}
	return "(global home)"
}

// ----- tool registration -----

func registerMCPTools(s *mcp.Server, app *App) {
	mustRegister := func(t mcp.Tool, h mcp.Handler) {
		if err := s.RegisterTool(t, h); err != nil {
			fmt.Fprintln(os.Stderr, "warn: register tool", t.Name, err)
		}
	}

	mustRegister(mcp.Tool{
		Name:        "uta_providers_list",
		Description: "List detected agent providers (claude, gemini, declarative YAML descriptors) with their capabilities and versions.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false}`),
	}, func(ctx context.Context, _ json.RawMessage) mcp.ToolResult {
		dets := app.Registry.DetectAll(ctx)
		out := []map[string]any{}
		for _, name := range app.Registry.Names() {
			d := dets[name]
			caps := make([]string, 0, len(d.Capabilities))
			for _, c := range d.Capabilities {
				caps = append(caps, string(c))
			}
			row := map[string]any{
				"name":         name,
				"available":    d.Available,
				"version":      d.Version,
				"binary_path":  d.BinaryPath,
				"capabilities": caps,
				"notes":        d.Notes,
			}
			if d.Err != nil {
				row["error"] = d.Err.Error()
			}
			out = append(out, row)
		}
		return mcp.TextResult(jsonDump(out))
	})

	mustRegister(mcp.Tool{
		Name:        "uta_ideas_list",
		Description: "List ideas on the per-project improvement backlog. Returns severity-sorted entries.",
		InputSchema: json.RawMessage(`{
			"type":"object",
			"properties":{
				"status":{"type":"string","enum":["proposed","accepted","in_progress","done","failed","rejected"]},
				"severity":{"type":"string","enum":["high","medium","low","info"]},
				"limit":{"type":"integer","minimum":1,"maximum":500}
			},
			"additionalProperties":false
		}`),
	}, func(ctx context.Context, args json.RawMessage) mcp.ToolResult {
		var p struct {
			Status   string `json:"status"`
			Severity string `json:"severity"`
			Limit    int    `json:"limit"`
		}
		if err := json.Unmarshal(args, &p); err != nil {
			return mcp.ArgError("%v", err)
		}
		board := improve.NewBoard(app.Store)
		ideas, err := board.List(improve.ListOptions{
			Status: p.Status, Severity: p.Severity, Limit: p.Limit,
		})
		if err != nil {
			return mcp.ErrorResult("list ideas: " + err.Error())
		}
		return mcp.TextResult(jsonDump(ideas))
	})

	mustRegister(mcp.Tool{
		Name:        "uta_ideas_add",
		Description: "Manually add an idea to the project backlog. Title is required; severity defaults to medium.",
		InputSchema: json.RawMessage(`{
			"type":"object",
			"properties":{
				"title":{"type":"string","minLength":1},
				"body":{"type":"string"},
				"severity":{"type":"string","enum":["high","medium","low","info"]},
				"tags":{"type":"array","items":{"type":"string"}}
			},
			"required":["title"],
			"additionalProperties":false
		}`),
	}, func(ctx context.Context, args json.RawMessage) mcp.ToolResult {
		var p struct {
			Title    string   `json:"title"`
			Body     string   `json:"body"`
			Severity string   `json:"severity"`
			Tags     []string `json:"tags"`
		}
		if err := json.Unmarshal(args, &p); err != nil {
			return mcp.ArgError("%v", err)
		}
		if strings.TrimSpace(p.Title) == "" {
			return mcp.ArgError("title is required")
		}
		idea := &improve.Idea{
			Title: p.Title, Body: p.Body, Severity: p.Severity,
			Source: "mcp", Tags: p.Tags,
		}
		board := improve.NewBoard(app.Store)
		if err := board.Insert(idea); err != nil {
			return mcp.ErrorResult("insert: " + err.Error())
		}
		return mcp.TextResult(jsonDump(idea))
	})

	mustRegister(mcp.Tool{
		Name:        "uta_ideas_show",
		Description: "Fetch one idea by id.",
		InputSchema: json.RawMessage(`{
			"type":"object",
			"properties":{"id":{"type":"string"}},
			"required":["id"],
			"additionalProperties":false
		}`),
	}, func(ctx context.Context, args json.RawMessage) mcp.ToolResult {
		var p struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(args, &p); err != nil {
			return mcp.ArgError("%v", err)
		}
		board := improve.NewBoard(app.Store)
		idea, err := board.Get(p.ID)
		if err != nil {
			return mcp.ErrorResult("get: " + err.Error())
		}
		return mcp.TextResult(jsonDump(idea))
	})

	mustRegister(mcp.Tool{
		Name:        "uta_sessions_list",
		Description: "List recent orchestration sessions (project-scoped if launched inside a project, else global).",
		InputSchema: json.RawMessage(`{
			"type":"object",
			"properties":{
				"limit":{"type":"integer","minimum":1,"maximum":500},
				"offset":{"type":"integer","minimum":0},
				"status":{"type":"string"}
			},
			"additionalProperties":false
		}`),
	}, func(ctx context.Context, args json.RawMessage) mcp.ToolResult {
		var p struct {
			Limit  int    `json:"limit"`
			Offset int    `json:"offset"`
			Status string `json:"status"`
		}
		if err := json.Unmarshal(args, &p); err != nil {
			return mcp.ArgError("%v", err)
		}
		if p.Limit <= 0 {
			p.Limit = 50
		}
		sessions, err := app.Store.ListSessions(p.Limit, p.Offset, p.Status)
		if err != nil {
			return mcp.ErrorResult("list: " + err.Error())
		}
		// Goals are operator-typed and occasionally contain pasted
		// credentials; scrub like the trajectory path does.
		return mcp.TextResult(redactedJSONDump(sessions))
	})

	mustRegister(mcp.Tool{
		Name:        "uta_trajectory_get",
		Description: "Return every recorded event for one session, in order.",
		InputSchema: json.RawMessage(`{
			"type":"object",
			"properties":{
				"session_id":{"type":"string"},
				"limit":{"type":"integer","minimum":1,"maximum":10000},
				"offset":{"type":"integer","minimum":0}
			},
			"required":["session_id"],
			"additionalProperties":false
		}`),
	}, func(ctx context.Context, args json.RawMessage) mcp.ToolResult {
		var p struct {
			SessionID string `json:"session_id"`
			Limit     int    `json:"limit"`
			Offset    int    `json:"offset"`
		}
		if err := json.Unmarshal(args, &p); err != nil {
			return mcp.ArgError("%v", err)
		}
		// Accept the short ids uta_sessions_list output displays.
		sid, rerr := app.Store.ResolveSessionID(p.SessionID)
		if rerr != nil {
			return mcp.ErrorResult(rerr.Error())
		}
		events, err := app.Store.ListEvents(sid, p.Limit, p.Offset)
		if err != nil {
			return mcp.ErrorResult("events: " + err.Error())
		}
		// Trajectory payloads carry raw provider output — scrub
		// secret-shaped strings before handing them to an MCP client.
		// The client is another agent, possibly forwarding to a remote
		// model; the same posture as `uta exportdb` applies.
		return mcp.TextResult(redactedJSONDump(events))
	})

	mustRegister(mcp.Tool{
		Name:        "uta_audit",
		Description: "Run a Reflector audit on a directory. Audit-only (no auto-fix) for safety from an MCP client. Returns the FindingsReport JSON.",
		InputSchema: json.RawMessage(`{
			"type":"object",
			"properties":{
				"path":{"type":"string","description":"directory to audit"},
				"critics":{"type":"array","items":{"type":"string"},"description":"persona ids; defaults to trail-of-bits + openzeppelin-style"},
				"worker":{"type":"string","description":"provider running the critics; defaults to first detected"},
				"timeout":{"type":"string","description":"per-critic timeout (Go duration, e.g. 10m)"}
			},
			"required":["path"],
			"additionalProperties":false
		}`),
	}, func(ctx context.Context, args json.RawMessage) mcp.ToolResult {
		var p struct {
			Path    string   `json:"path"`
			Critics []string `json:"critics"`
			Worker  string   `json:"worker"`
			Timeout string   `json:"timeout"`
		}
		if err := json.Unmarshal(args, &p); err != nil {
			return mcp.ArgError("%v", err)
		}
		if strings.TrimSpace(p.Path) == "" {
			return mcp.ArgError("path is required")
		}
		selected, err := resolveCritics(app, p.Critics)
		if err != nil {
			return mcp.ErrorResult(err.Error())
		}
		worker := p.Worker
		if worker == "" {
			dets := app.Registry.DetectAll(ctx)
			avail := availableProviders(app.Registry.Names(), dets)
			if len(avail) == 0 {
				return mcp.ErrorResult("no provider on PATH")
			}
			worker = avail[0]
		}
		t, _ := time.ParseDuration(p.Timeout)
		if t == 0 {
			t = 10 * time.Minute
		}
		bus := trajectory.NewBus()
		recorder := trajectory.NewRecorder(app.Store)
		sup := engine.New(engine.Deps{
			Store: app.Store, Blobs: app.Blobs, Recorder: recorder,
			Bus: bus, Registry: app.Registry,
			Memory:     memory.NewFromEnv(app.Store.DB),
			Whiteboard: whiteboard.New(app.Store.DB),
		})
		req := engine.ReflectorRequest{
			Goal:          "MCP-triggered audit of " + p.Path,
			InputSummary:  "the codebase rooted at " + p.Path,
			Critics:       selected,
			MaxIterations: 1,
			DefaultWorker: worker,
			CriticTimeout: t,
			RunTimeout:    t + 5*time.Minute,
			Workdir:       p.Path,
			ContextDir:    app.ContextDir,
		}
		result, err := sup.RunReflector(ctx, req)
		bus.Shutdown()
		if err != nil {
			return mcp.ErrorResult("audit: " + err.Error())
		}
		out := map[string]any{
			"session_id":      result.SessionID,
			"status":          result.Status,
			"stopped_because": result.StoppedBecause,
			"findings":        result.FinalFindings,
		}
		return mcp.TextResult(jsonDump(out))
	})

	mustRegister(mcp.Tool{
		Name:        "uta_run",
		Description: "Run an ad-hoc goal through the supervisor (fan-out strategy by default). One round trip, one final answer.",
		InputSchema: json.RawMessage(`{
			"type":"object",
			"properties":{
				"goal":{"type":"string","minLength":1},
				"worker":{"type":"string"},
				"max_subtasks":{"type":"integer","minimum":1,"maximum":12},
				"timeout":{"type":"string"}
			},
			"required":["goal"],
			"additionalProperties":false
		}`),
	}, func(ctx context.Context, args json.RawMessage) mcp.ToolResult {
		var p struct {
			Goal        string `json:"goal"`
			Worker      string `json:"worker"`
			MaxSubtasks int    `json:"max_subtasks"`
			Timeout     string `json:"timeout"`
		}
		if err := json.Unmarshal(args, &p); err != nil {
			return mcp.ArgError("%v", err)
		}
		if strings.TrimSpace(p.Goal) == "" {
			return mcp.ArgError("goal is required")
		}
		worker := p.Worker
		if worker == "" {
			dets := app.Registry.DetectAll(ctx)
			avail := availableProviders(app.Registry.Names(), dets)
			if len(avail) == 0 {
				return mcp.ErrorResult("no provider on PATH")
			}
			worker = avail[0]
		}
		timeout, _ := time.ParseDuration(p.Timeout)
		if timeout == 0 {
			timeout = 10 * time.Minute
		}
		if p.MaxSubtasks == 0 {
			p.MaxSubtasks = 4
		}
		bus := trajectory.NewBus()
		recorder := trajectory.NewRecorder(app.Store)
		sup := engine.New(engine.Deps{
			Store: app.Store, Blobs: app.Blobs, Recorder: recorder,
			Bus: bus, Registry: app.Registry,
			Memory:     memory.NewFromEnv(app.Store.DB),
			Whiteboard: whiteboard.New(app.Store.DB),
		})
		res, err := sup.Run(ctx, engine.RunRequest{
			Goal:           p.Goal,
			WorkerName:     worker,
			MaxParallel:    p.MaxSubtasks,
			MaxSubtasks:    p.MaxSubtasks,
			SubtaskTimeout: timeout,
			RunTimeout:     timeout + 5*time.Minute,
		})
		bus.Shutdown()
		if err != nil {
			return mcp.ErrorResult("run: " + err.Error())
		}
		return mcp.TextResult(jsonDump(map[string]any{
			"session_id":   res.SessionID,
			"status":       res.Status,
			"final_answer": res.FinalAnswer,
		}))
	})

	mustRegister(mcp.Tool{
		Name:        "uta_improve_pick",
		Description: "Inspect (do not execute) the next idea the improve loop would pick. Useful for an MCP client that wants to schedule its own execution.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false}`),
	}, func(ctx context.Context, _ json.RawMessage) mcp.ToolResult {
		board := improve.NewBoard(app.Store)
		idea, err := board.NextActionable()
		if err != nil {
			return mcp.TextResult(`{"idea":null,"reason":"backlog empty"}`)
		}
		return mcp.TextResult(jsonDump(idea))
	})

	mustRegister(mcp.Tool{
		Name:        "uta_whiteboard_set",
		Description: "Post a note to the inter-agent whiteboard. The next mode's planner sees the latest note for each key in its prompt context, so this is the channel for leaving handoff hints, blockers, or shared flags. value_json must be valid JSON; key is free-form.",
		InputSchema: json.RawMessage(`{
			"type":"object",
			"properties":{
				"key":{"type":"string","minLength":1},
				"value_json":{"type":"string","minLength":1,"description":"Raw JSON text — object, array, string literal, number, etc."},
				"author_mode":{"type":"string","description":"Mode name on whose behalf the note is posted (e.g. \"ops\", \"dev\"). Optional."},
				"author_session":{"type":"string","description":"Optional session id for traceability."}
			},
			"required":["key","value_json"],
			"additionalProperties":false
		}`),
	}, func(ctx context.Context, args json.RawMessage) mcp.ToolResult {
		var p struct {
			Key           string `json:"key"`
			ValueJSON     string `json:"value_json"`
			AuthorMode    string `json:"author_mode"`
			AuthorSession string `json:"author_session"`
		}
		if err := json.Unmarshal(args, &p); err != nil {
			return mcp.ArgError("%v", err)
		}
		wb := whiteboard.New(app.Store.DB)
		id, err := wb.Set(whiteboard.Entry{
			Key:           p.Key,
			ValueJSON:     p.ValueJSON,
			AuthorMode:    p.AuthorMode,
			AuthorSession: p.AuthorSession,
		})
		if err != nil {
			return mcp.ErrorResult("whiteboard set: " + err.Error())
		}
		return mcp.TextResult(jsonDump(map[string]any{
			"id":          id,
			"key":         p.Key,
			"author_mode": p.AuthorMode,
		}))
	})

	mustRegister(mcp.Tool{
		Name:        "uta_whiteboard_get",
		Description: "Read the latest note for one key on the inter-agent whiteboard. Returns null when no entry exists for the key yet.",
		InputSchema: json.RawMessage(`{
			"type":"object",
			"properties":{"key":{"type":"string","minLength":1}},
			"required":["key"],
			"additionalProperties":false
		}`),
	}, func(ctx context.Context, args json.RawMessage) mcp.ToolResult {
		var p struct {
			Key string `json:"key"`
		}
		if err := json.Unmarshal(args, &p); err != nil {
			return mcp.ArgError("%v", err)
		}
		wb := whiteboard.New(app.Store.DB)
		entry, found, err := wb.Get(p.Key)
		if err != nil {
			return mcp.ErrorResult("whiteboard get: " + err.Error())
		}
		if !found {
			return mcp.TextResult(`null`)
		}
		return mcp.TextResult(jsonDump(map[string]any{
			"id":             entry.ID,
			"key":            entry.Key,
			"value_json":     entry.ValueJSON,
			"author_mode":    entry.AuthorMode,
			"author_session": entry.AuthorSession,
			"ts":             entry.Ts.UTC().Format(time.RFC3339Nano),
		}))
	})

	mustRegister(mcp.Tool{
		Name:        "uta_whiteboard_list",
		Description: "List the latest note per distinct key on the inter-agent whiteboard, newest-first. Use limit to cap the response.",
		InputSchema: json.RawMessage(`{
			"type":"object",
			"properties":{"limit":{"type":"integer","minimum":1,"maximum":500}},
			"additionalProperties":false
		}`),
	}, func(ctx context.Context, args json.RawMessage) mcp.ToolResult {
		var p struct {
			Limit int `json:"limit"`
		}
		if len(args) > 0 {
			if err := json.Unmarshal(args, &p); err != nil {
				return mcp.ArgError("%v", err)
			}
		}
		wb := whiteboard.New(app.Store.DB)
		entries, err := wb.List(p.Limit)
		if err != nil {
			return mcp.ErrorResult("whiteboard list: " + err.Error())
		}
		out := make([]map[string]any, 0, len(entries))
		for _, e := range entries {
			out = append(out, map[string]any{
				"id":             e.ID,
				"key":            e.Key,
				"value_json":     e.ValueJSON,
				"author_mode":    e.AuthorMode,
				"author_session": e.AuthorSession,
				"ts":             e.Ts.UTC().Format(time.RFC3339Nano),
			})
		}
		return mcp.TextResult(jsonDump(out))
	})
}

// redactedJSONDump marshals v like jsonDump, then scrubs secret-shaped
// strings from the serialized form. Operating on the final JSON (rather
// than walking the struct) means every nested payload — including raw
// provider events — gets the same pass with no per-type plumbing.
func redactedJSONDump(v any) string {
	clean, _ := redact.String(jsonDump(v))
	return clean
}

func jsonDump(v any) string {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Sprintf(`{"_marshal_error":%q}`, err.Error())
	}
	return string(b)
}
