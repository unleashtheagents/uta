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
	"github.com/unleashtheagents/uta/internal/trajectory"
	"github.com/unleashtheagents/uta/internal/version"
)

func newServeCmd() *cobra.Command {
	var mcpMode bool
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "expose uta as a server (MCP over stdio)",
		Long: `uta serve --mcp speaks JSON-RPC 2.0 over stdin/stdout per the Model
Context Protocol spec. Any MCP-aware client (Claude Code, IDEs, other
agents) can launch this process and call into uta's tools.

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

Project-aware: if uta is launched inside a project tree, ideas / sessions
/ trajectories are project-scoped. Otherwise they're global.

Generic by design: every uta capability that has a CLI surface should
eventually have an MCP tool surface. This release ships the read-heavy
subset plus audit + run (which are the high-value write operations).
Mutation surface stays narrow on purpose — the MCP client model is one
of cooperation, not blanket trust.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if !mcpMode {
				return errors.New("only --mcp is implemented in this release")
			}
			app, err := newApp(cmd.Context())
			if err != nil {
				return err
			}
			defer app.Close()

			server := mcp.NewServer("uta", version.Version)
			registerMCPTools(server, app)

			fmt.Fprintf(cmd.ErrOrStderr(), "uta MCP server ready · proto=%s · tools=%d · project=%s\n",
				mcp.ProtocolVersion, mcpToolCount(server), mcpProjectLabel(app))

			ctx, cancel := signalContext(cmd.Context())
			defer cancel()
			return server.Serve(ctx, cmd.InOrStdin(), cmd.OutOrStdout())
		},
	}
	cmd.Flags().BoolVar(&mcpMode, "mcp", false, "speak MCP (JSON-RPC 2.0) on stdin/stdout")
	return cmd
}

func mcpToolCount(s *mcp.Server) int {
	// Cheap reflection-free count via the listTools method? We don't expose
	// the list, so just call tools/list via a dry handle… For simplicity,
	// return 0 and let stderr be honest about counts. (This is a no-op
	// helper for the welcome banner.)
	return -1
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
			out = append(out, map[string]any{
				"name":         name,
				"available":    d.Available,
				"version":      d.Version,
				"binary_path":  d.BinaryPath,
				"capabilities": caps,
				"notes":        d.Notes,
			})
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
		_ = json.Unmarshal(args, &p)
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
				"status":{"type":"string"}
			},
			"additionalProperties":false
		}`),
	}, func(ctx context.Context, args json.RawMessage) mcp.ToolResult {
		var p struct {
			Limit  int    `json:"limit"`
			Status string `json:"status"`
		}
		_ = json.Unmarshal(args, &p)
		sessions, err := app.Store.ListSessions(p.Limit, p.Status)
		if err != nil {
			return mcp.ErrorResult("list: " + err.Error())
		}
		return mcp.TextResult(jsonDump(sessions))
	})

	mustRegister(mcp.Tool{
		Name:        "uta_trajectory_get",
		Description: "Return every recorded event for one session, in order.",
		InputSchema: json.RawMessage(`{
			"type":"object",
			"properties":{"session_id":{"type":"string"}},
			"required":["session_id"],
			"additionalProperties":false
		}`),
	}, func(ctx context.Context, args json.RawMessage) mcp.ToolResult {
		var p struct {
			SessionID string `json:"session_id"`
		}
		if err := json.Unmarshal(args, &p); err != nil {
			return mcp.ArgError("%v", err)
		}
		events, err := app.Store.ListEvents(p.SessionID)
		if err != nil {
			return mcp.ErrorResult("events: " + err.Error())
		}
		return mcp.TextResult(jsonDump(events))
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
}

func jsonDump(v any) string {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Sprintf(`{"_marshal_error":%q}`, err.Error())
	}
	return string(b)
}
