package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/unleashtheagents/uta/internal/hitl"
	"github.com/unleashtheagents/uta/internal/skills"
	"github.com/unleashtheagents/uta/internal/store"
	"github.com/unleashtheagents/uta/internal/version"
)

func isSafeHITLID(s string) bool {
	s = strings.TrimSpace(s)
	if s == "" || s == "." || s == ".." {
		return false
	}
	return !strings.ContainsAny(s, "/\\\x00") && !strings.Contains(s, "..")
}

func isSameOriginLocalRequest(r *http.Request) bool {
	if sec := r.Header.Get("Sec-Fetch-Site"); sec != "" && sec != "same-origin" && sec != "none" {
		return false
	}
	if origin := r.Header.Get("Origin"); origin != "" {
		u, err := url.Parse(origin)
		if err != nil {
			return false
		}
		host := u.Hostname()
		if host != "127.0.0.1" && host != "localhost" && host != "::1" {
			return false
		}
	}
	return true
}

func runWebServer(ctx context.Context, app *App, port int, errOut io.Writer) error {
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	mux := http.NewServeMux()

	var (
		provMu       sync.Mutex
		provCachedAt time.Time
		provCached   []map[string]any
	)
	const provCacheTTL = 30 * time.Second

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" && r.URL.Path != "/index.html" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(dashWebHTML(app.ProjectName)))
	})

	mux.HandleFunc("/api/dash", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		data := collectDashData(app, errOut, 20, 20)
		modeFilter := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("mode")))
		tagKey := r.URL.Query().Get("tag_key")
		if modeFilter != "" {
			_ = emitDashJSON(w, data, modeFilter, tagKey)
			return
		}
		panes := make(map[string]any, len(paneRegistry))
		for name, spec := range paneRegistry {
			panes[name] = spec.build(data, tagKey)
		}
		for _, name := range panesToRender(data.Profiles, data.Stats) {
			if _, exists := panes[name]; !exists {
				panes[name] = buildGenericPane(data, tagKey, name)
			}
		}
		totalSessions := 0
		for _, ms := range data.Stats {
			if ms != nil {
				totalSessions += ms.TotalSessions
			}
		}
		envelope := make(map[string]any, len(panes)+2)
		for k, v := range panes {
			envelope[k] = v
		}
		envelope["panes"] = panes
		envelope["summary"] = map[string]any{
			"sessions_total": totalSessions,
			"ideas_total":    len(data.Ideas),
			"alerts_24h":     len(data.Alerts),
		}
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		_ = enc.Encode(envelope)
	})

	mux.HandleFunc("/api/providers", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		provMu.Lock()
		if provCached == nil || time.Since(provCachedAt) >= provCacheTTL {
			dets := app.Registry.DetectAll(r.Context())
			rows := make([]map[string]any, 0, len(dets))
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
				rows = append(rows, row)
			}
			provCached = rows
			provCachedAt = time.Now()
		}
		out := provCached
		provMu.Unlock()
		_ = json.NewEncoder(w).Encode(out)
	})

	mux.HandleFunc("/api/sessions", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		sessions, err := app.Store.ListSessions(50, 0, "")
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if sessions == nil {
			sessions = []store.Session{}
		}
		_, _ = io.WriteString(w, redactedJSONDump(sessions))
	})

	mux.HandleFunc("/api/hitl/pending", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		pending, err := hitl.ListPending(app.StateDir)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if pending == nil {
			pending = []*hitl.Pending{}
		}
		_ = json.NewEncoder(w).Encode(pending)
	})

	mux.HandleFunc("/api/hitl/action", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if !isSameOriginLocalRequest(r) {
			http.Error(w, "cross-origin request denied", http.StatusForbidden)
			return
		}
		var req struct {
			SessionID string `json:"session_id"`
			ActionID  string `json:"action_id"`
			Deny      bool   `json:"deny"`
			Reason    string `json:"reason"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if !isSafeHITLID(req.SessionID) || !isSafeHITLID(req.ActionID) {
			http.Error(w, "invalid session_id or action_id", http.StatusBadRequest)
			return
		}
		sessionDir := hitl.SessionDir(app.StateDir, req.SessionID)
		if req.Deny {
			reason := req.Reason
			if reason == "" {
				reason = "denied via web portal"
			}
			if err := hitl.WriteDenial(sessionDir, req.ActionID, reason); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
		} else {
			if err := hitl.WriteApproval(sessionDir, req.ActionID); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]bool{"ok": true})
	})

	mux.HandleFunc("/api/replay", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		sessionID := r.URL.Query().Get("session")
		if sessionID == "" {
			http.Error(w, "missing session query parameter", http.StatusBadRequest)
			return
		}
		resolvedID, err := app.Store.ResolveSessionID(sessionID)
		if err != nil {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}
		events, err := app.Store.ListEvents(resolvedID, 0, 0)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if events == nil {
			events = []store.EventRow{}
		}
		_, _ = io.WriteString(w, redactedJSONDump(events))
	})

	mux.HandleFunc("/api/nebius", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		rep := buildNebiusStatus(app.ProjectRoot, app.GlobalHome)
		_ = json.NewEncoder(w).Encode(rep)
	})

	mux.HandleFunc("/api/skills", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		cat := skills.Discover(app.ProjectRoot, app.GlobalHome)
		_ = json.NewEncoder(w).Encode(cat)
	})

	server := &http.Server{
		Addr:         addr,
		Handler:      mux,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 10 * time.Second,
	}

	fmt.Fprintf(errOut, "[uta web] dashboard server listening at http://%s · version=%s · project=%s\n",
		addr, version.Version, mcpProjectLabel(app))

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	}()

	err := server.ListenAndServe()
	if err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}

func dashWebHTML(projectName string) string {
	proj := projectName
	if proj == "" {
		proj = "Global Scope"
	}
	ver := version.Version
	if ver == "" {
		ver = "0.14.0"
	}
	return fmt.Sprintf(`<!DOCTYPE html>
<html lang="en">
<head>
  <meta charset="UTF-8">
  <meta name="viewport" content="width=device-width, initial-scale=1.0">
  <title>uta — Agentic OS Dashboard</title>
  <style>
    :root {
      --bg: #090c10;
      --panel-bg: #11161d;
      --card-bg: #161c24;
      --border: #232c3b;
      --text: #e6edf3;
      --text-muted: #8b949e;
      --mint: #5cffa8;
      --purple: #7c5cff;
      --blue: #38bdf8;
      --amber: #fbbf24;
      --danger: #f85149;
      --font-sans: -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, sans-serif;
      --font-mono: "JetBrains Mono", "Fira Code", monospace;
    }
    * { box-sizing: border-box; margin: 0; padding: 0; }
    body {
      font-family: var(--font-sans);
      background: var(--bg);
      color: var(--text);
      line-height: 1.5;
      padding: 24px;
      min-height: 100vh;
    }
    header {
      display: flex;
      justify-content: space-between;
      align-items: center;
      padding-bottom: 18px;
      border-bottom: 1px solid var(--border);
      margin-bottom: 24px;
      flex-wrap: wrap;
      gap: 12px;
    }
    .logo-group { display: flex; align-items: center; gap: 12px; }
    .logo { font-weight: 700; font-size: 1.35rem; color: #fff; letter-spacing: -0.02em; display: flex; align-items: center; gap: 10px; }
    .pulse { width: 9px; height: 9px; border-radius: 50%%; background: var(--mint); display: inline-block; box-shadow: 0 0 10px var(--mint); }
    .badge { background: rgba(124, 92, 255, 0.12); border: 1px solid rgba(124, 92, 255, 0.3); padding: 4px 10px; border-radius: 6px; font-size: 0.78rem; font-family: var(--font-mono); color: var(--purple); }
    .badge-scope { background: #1c2333; border: 1px solid var(--border); color: var(--text-muted); }
    .meta-time { font-family: var(--font-mono); font-size: 0.78rem; color: var(--text-muted); }

    /* Nav Tabs */
    .tabs { display: flex; gap: 8px; margin-bottom: 20px; border-bottom: 1px solid var(--border); padding-bottom: 12px; overflow-x: auto; }
    .tab-btn { background: transparent; border: 1px solid transparent; color: var(--text-muted); padding: 7px 14px; border-radius: 6px; font-size: 0.85rem; font-weight: 500; cursor: pointer; transition: all 0.15s ease; white-space: nowrap; }
    .tab-btn:hover { color: var(--text); background: rgba(255,255,255,0.03); }
    .tab-btn.active { color: var(--mint); background: rgba(92, 255, 168, 0.08); border-color: rgba(92, 255, 168, 0.25); }

    /* Key Stats Grid */
    .grid { display: grid; grid-template-columns: repeat(auto-fit, minmax(220px, 1fr)); gap: 14px; margin-bottom: 24px; }
    .card { background: var(--card-bg); border: 1px solid var(--border); border-radius: 8px; padding: 16px; position: relative; overflow: hidden; }
    .card::before { content: ""; position: absolute; top: 0; left: 0; right: 0; height: 2px; background: linear-gradient(90deg, var(--purple), var(--mint)); opacity: 0.6; }
    .card h3 { font-size: 0.75rem; text-transform: uppercase; letter-spacing: 0.08em; color: var(--text-muted); margin-bottom: 6px; font-family: var(--font-mono); }
    .card .value { font-size: 1.85rem; font-weight: 700; color: #fff; font-family: var(--font-mono); }
    .card .subtext { font-size: 0.75rem; color: var(--text-muted); margin-top: 4px; }

    /* Tables & Panels */
    .section-panel { background: var(--panel-bg); border: 1px solid var(--border); border-radius: 8px; padding: 20px; margin-bottom: 24px; }
    .section-head { display: flex; justify-content: space-between; align-items: center; margin-bottom: 16px; }
    .section-title { font-size: 1rem; font-weight: 600; color: #fff; display: flex; align-items: center; gap: 8px; }
    .section-title span { color: var(--mint); font-family: var(--font-mono); font-size: 0.85rem; }

    table { width: 100%%; border-collapse: collapse; font-size: 0.88rem; }
    th, td { text-align: left; padding: 11px 14px; border-bottom: 1px solid var(--border); }
    th { color: var(--text-muted); font-weight: 600; font-size: 0.75rem; font-family: var(--font-mono); text-transform: uppercase; letter-spacing: 0.05em; background: rgba(0,0,0,0.2); }
    tr:hover td { background: rgba(255,255,255,0.015); }
    code { font-family: var(--font-mono); font-size: 0.82rem; color: var(--blue); background: rgba(56, 189, 248, 0.08); padding: 2px 6px; border-radius: 4px; }

    .status { display: inline-block; padding: 2px 8px; border-radius: 6px; font-size: 0.72rem; font-weight: 600; font-family: var(--font-mono); text-transform: uppercase; }
    .status-completed { background: rgba(92, 255, 168, 0.12); color: var(--mint); border: 1px solid rgba(92, 255, 168, 0.3); }
    .status-running { background: rgba(56, 189, 248, 0.12); color: var(--blue); border: 1px solid rgba(56, 189, 248, 0.3); }
    .status-failed { background: rgba(248, 81, 73, 0.12); color: var(--danger); border: 1px solid rgba(248, 81, 73, 0.3); }
    .status-partial { background: rgba(251, 191, 36, 0.12); color: var(--amber); border: 1px solid rgba(251, 191, 36, 0.3); }

    /* Provider Grid */
    .providers-grid { display: grid; grid-template-columns: repeat(auto-fit, minmax(260px, 1fr)); gap: 14px; }
    .prov-card { background: var(--card-bg); border: 1px solid var(--border); border-radius: 8px; padding: 16px; }
    .prov-head { display: flex; justify-content: space-between; align-items: center; margin-bottom: 10px; }
    .prov-name { font-weight: 700; font-size: 1rem; color: #fff; font-family: var(--font-mono); }
    .prov-ver { font-size: 0.78rem; font-family: var(--font-mono); color: var(--text-muted); }
    .prov-caps { display: flex; flex-wrap: wrap; gap: 4px; margin-top: 10px; }
    .cap-pill { font-size: 0.7rem; font-family: var(--font-mono); background: rgba(124, 92, 255, 0.1); color: var(--purple); border: 1px solid rgba(124, 92, 255, 0.25); padding: 2px 6px; border-radius: 4px; }

    /* Features / Modes Cards */
    .modes-grid { display: grid; grid-template-columns: repeat(auto-fit, minmax(240px, 1fr)); gap: 14px; }
    .mode-card { background: var(--card-bg); border: 1px solid var(--border); border-radius: 8px; padding: 16px; }
    .mode-tag { font-family: var(--font-mono); font-size: 0.7rem; color: var(--mint); letter-spacing: 0.08em; text-transform: uppercase; margin-bottom: 6px; }
    .mode-h { font-size: 0.95rem; font-weight: 600; color: #fff; margin-bottom: 8px; }
    .mode-desc { font-size: 0.8rem; color: var(--text-muted); line-height: 1.45; }

    /* Command Cheat Sheet */
    .cmd-list { display: grid; grid-template-columns: repeat(auto-fit, minmax(320px, 1fr)); gap: 12px; }
    .cmd-item { background: #0c1017; border: 1px solid var(--border); border-radius: 6px; padding: 12px; }
    .cmd-title { font-size: 0.82rem; font-weight: 600; color: var(--mint); margin-bottom: 4px; font-family: var(--font-mono); }
    .cmd-desc { font-size: 0.78rem; color: var(--text-muted); margin-bottom: 8px; }
    .cmd-code { font-family: var(--font-mono); font-size: 0.8rem; color: var(--text); background: rgba(0,0,0,0.4); padding: 6px 10px; border-radius: 4px; display: block; overflow-x: auto; border: 1px solid rgba(255,255,255,0.05); }

    .tab-content { display: none; }
    .tab-content.active { display: block; }
  </style>
</head>
<body>
  <header>
    <div class="logo-group">
      <div class="logo">
        <span class="pulse"></span> uta dashboard
      </div>
      <span class="badge">%s</span>
      <span class="badge badge-scope">v%s · agentic-os</span>
    </div>
    <div class="meta-time" id="last-updated">Updating...</div>
  </header>

  <div class="grid">
    <div class="card">
      <h3>Active Sessions</h3>
      <div class="value" id="stat-sessions">0</div>
      <div class="subtext">Recorded in local SQLite</div>
    </div>
    <div class="card">
      <h3>Detected Providers</h3>
      <div class="value" id="stat-providers">0</div>
      <div class="subtext">Nebius (Nemotron 3), Claude Code, Antigravity (agy), Gemini</div>
    </div>
    <div class="card">
      <h3>Sentinel Alerts (24h)</h3>
      <div class="value" id="stat-alerts">0</div>
      <div class="subtext">Zero policy violations</div>
    </div>
    <div class="card">
      <h3>Ideas Backlog</h3>
      <div class="value" id="stat-ideas">0</div>
      <div class="subtext">Pending refinement</div>
    </div>
  </div>

  <div class="tabs">
    <button class="tab-btn active" onclick="switchTab('sessions')">Sessions &amp; Trajectories</button>
    <button class="tab-btn" onclick="switchTab('providers')">Agent Providers</button>
    <button class="tab-btn" onclick="switchTab('modes')">Mission Modes</button>
    <button class="tab-btn" onclick="switchTab('commands')">CLI Capabilities</button>
  </div>

  <!-- Tab 1: Sessions -->
  <div id="tab-sessions" class="tab-content active">
    <div class="section-panel">
      <div class="section-head">
        <div class="section-title"><span>/</span> Recent Orchestration Sessions</div>
      </div>
      <table>
        <thead>
          <tr>
            <th>Session ID</th>
            <th>Goal</th>
            <th>Worker</th>
            <th>Status</th>
            <th>Mode</th>
          </tr>
        </thead>
        <tbody id="sessions-table">
          <tr><td colspan="5" style="text-align: center; color: var(--text-muted);">Loading sessions...</td></tr>
        </tbody>
      </table>
    </div>
  </div>

  <!-- Tab 2: Providers -->
  <div id="tab-providers" class="tab-content">
    <div class="section-panel">
      <div class="section-head">
        <div class="section-title"><span>/</span> Detected Agent Provider CLIs</div>
      </div>
      <div class="providers-grid" id="providers-grid">
        <div style="color: var(--text-muted);">Scanning installed providers...</div>
      </div>
    </div>
  </div>

  <!-- Tab 3: Modes -->
  <div id="tab-modes" class="tab-content">
    <div class="section-panel">
      <div class="section-head">
        <div class="section-title"><span>/</span> Mission Profiles &amp; Operating Modes</div>
      </div>
      <div class="modes-grid">
        <div class="mode-card">
          <div class="mode-tag">Mode 01 · DEV</div>
          <div class="mode-h">Evaluations &amp; Replay</div>
          <div class="mode-desc">Build with evals running. Frame-by-frame trajectory replay, deterministic harness, and instant tool registration.</div>
        </div>
        <div class="mode-card">
          <div class="mode-tag">Mode 02 · OPS</div>
          <div class="mode-h">Guardrails &amp; Dialtone</div>
          <div class="mode-desc">Live agent registry, OTel telemetry, budget caps, Sentinel watcher, and 9-second out-of-band kill switch.</div>
        </div>
        <div class="mode-card">
          <div class="mode-tag">Mode 03 · RESEARCH</div>
          <div class="mode-h">Lab &amp; Scenario Sweeps</div>
          <div class="mode-desc">Hypothesis ledger, long-horizon scenario sweeps across seeds, cost-capability frontier analysis with error bars.</div>
        </div>
        <div class="mode-card">
          <div class="mode-tag">Mode 04 · AUDIT</div>
          <div class="mode-h">SHA-256 Immutable Ledger</div>
          <div class="mode-desc">Hash-chained append-only log mapped to EU AI Act Article 12, NIST AI RMF, ISO 42001, and DORA reporting.</div>
        </div>
      </div>
    </div>
  </div>

  <!-- Tab 4: Commands -->
  <div id="tab-commands" class="tab-content">
    <div class="section-panel">
      <div class="section-head">
        <div class="section-title"><span>/</span> Essential uta CLI Workflows</div>
      </div>
      <div class="cmd-list">
        <div class="cmd-item">
          <div class="cmd-title">uta nebius</div>
          <div class="cmd-desc">Nebius Token Factory × NVIDIA Nemotron 90/10 coordinator status, model tiers, and goal routing.</div>
          <span class="cmd-code">uta nebius route "design multi-agent gateway"</span>
        </div>
        <div class="cmd-item">
          <div class="cmd-title">uta skills</div>
          <div class="cmd-desc">Discover hierarchical rules (AGENTS.md, CLAUDE.md, GEMINI.md) and progressive SKILL.md skills across agy, claude &amp; uta.</div>
          <span class="cmd-code">uta skills list</span>
        </div>
        <div class="cmd-item">
          <div class="cmd-title">uta pr</div>
          <div class="cmd-desc">Autonomous dev mission on dedicated Git branch with test gates and automatic GitHub PR creation.</div>
          <span class="cmd-code">uta pr -g "add rate limiting" --worker nebius --verify 'go test ./...'</span>
        </div>
        <div class="cmd-item">
          <div class="cmd-title">uta discuss</div>
          <div class="cmd-desc">Pits two agent LLMs against each other for rebuttals; moderator synthesizes consensus.</div>
          <span class="cmd-code">uta discuss -t "monolith vs microservices"</span>
        </div>
        <div class="cmd-item">
          <div class="cmd-title">uta serve --mcp</div>
          <div class="cmd-desc">Expose uta as an MCP server to Claude Code and Cursor with auto-generated client config.</div>
          <span class="cmd-code">uta serve --print-mcp-config claude-code > .mcp.json</span>
        </div>
        <div class="cmd-item">
          <div class="cmd-title">uta shell</div>
          <div class="cmd-desc">Persistent interactive terminal REPL with slash commands (/model, /tools, @gemini).</div>
          <span class="cmd-code">uta shell</span>
        </div>
        <div class="cmd-item">
          <div class="cmd-title">uta mission test</div>
          <div class="cmd-desc">Execute steer program against scripted golden responses with zero token cost.</div>
          <span class="cmd-code">uta mission test examples/hello.steer --script canned.yaml</span>
        </div>
        <div class="cmd-item">
          <div class="cmd-title">uta run --strategy dag</div>
          <div class="cmd-desc">Decompose goals into DAG orchestration with explicit subtask dependency graphs.</div>
          <span class="cmd-code">uta run -g "build pipeline" --strategy dag -y</span>
        </div>
        <div class="cmd-item">
          <div class="cmd-title">uta bench</div>
          <div class="cmd-desc">Benchmark latency (ms), token throughput (tok/s), and quality across all detected providers.</div>
          <span class="cmd-code">uta bench --prompt "Explain B-tree"</span>
        </div>
        <div class="cmd-item">
          <div class="cmd-title">uta replay</div>
          <div class="cmd-desc">Time-travel step-by-step trajectory replay of any recorded session.</div>
          <span class="cmd-code">uta replay 4f6c --delay-ms 200</span>
        </div>
      </div>
    </div>
  </div>

  <script>
    function esc(s) {
      return String(s == null ? '' : s)
        .replace(/&/g, '&amp;')
        .replace(/</g, '&lt;')
        .replace(/>/g, '&gt;')
        .replace(/"/g, '&quot;')
        .replace(/'/g, '&#39;');
    }

    function switchTab(tabId) {
      document.querySelectorAll('.tab-btn').forEach(btn => btn.classList.remove('active'));
      document.querySelectorAll('.tab-content').forEach(c => c.classList.remove('active'));
      
      const targetBtn = Array.from(document.querySelectorAll('.tab-btn')).find(b => b.getAttribute('onclick').includes(tabId));
      if (targetBtn) targetBtn.classList.add('active');
      
      const content = document.getElementById('tab-' + tabId);
      if (content) content.classList.add('active');
    }

    async function fetchDash() {
      try {
        const res = await fetch('/api/dash');
        const data = await res.json();
        
        let alertCount = 0;
        let ideaCount = 0;
        let sessionCount = 0;

        if (data.summary) {
          sessionCount = data.summary.sessions_total || 0;
          ideaCount = data.summary.ideas_total || 0;
          alertCount = data.summary.alerts_24h || 0;
        } else if (data.panes) {
          Object.values(data.panes).forEach(pane => {
            if (pane.sessions_total) sessionCount += pane.sessions_total;
            if (pane.ideas_total) ideaCount += pane.ideas_total;
          });
        }

        document.getElementById('stat-sessions').innerText = sessionCount;
        document.getElementById('stat-ideas').innerText = ideaCount;
        document.getElementById('stat-alerts').innerText = alertCount;

        const sRes = await fetch('/api/sessions');
        const sessions = await sRes.json();
        
        let sessionsHtml = '';
        if (sessions && sessions.length > 0) {
          sessionsHtml = sessions.map(s => {
            const st = esc(s.status || 'unknown').replace(/[^a-z0-9_-]/gi, '');
            const sid = s.id ? esc(s.id.substring(0, 8)) : '-';
            return '<tr>' +
              '<td><code>' + sid + '</code></td>' +
              '<td>' + esc(s.goal || '-') + '</td>' +
              '<td><code>' + esc(s.worker || '-') + '</code></td>' +
              '<td><span class="status status-' + st + '">' + esc(s.status || 'unknown') + '</span></td>' +
              '<td><code>' + esc(s.mode_name || 'default') + '</code></td>' +
            '</tr>';
          }).join('');
        } else {
          sessionsHtml = '<tr><td colspan="5" style="text-align: center; color: var(--text-muted);">No sessions recorded yet. Run <code>uta run</code> or <code>uta pr</code> to start.</td></tr>';
        }
        document.getElementById('sessions-table').innerHTML = sessionsHtml;

        // Fetch Providers
        const pRes = await fetch('/api/providers');
        const providers = await pRes.json();
        let provCount = 0;
        let provHtml = '';
        if (providers && Array.isArray(providers)) {
          provCount = providers.filter(p => p.available).length;
          provHtml = providers.map(p => {
            const isAvail = !!p.available;
            const caps = (p.capabilities || []).map(c => '<span class="cap-pill">' + esc(c) + '</span>').join(' ');
            return '<div class="prov-card">' +
              '<div class="prov-head">' +
                '<div class="prov-name">' + esc(p.name) + '</div>' +
                '<span class="status ' + (isAvail ? 'status-completed' : 'status-failed') + '">' + (isAvail ? 'available' : 'missing') + '</span>' +
              '</div>' +
              '<div class="prov-ver">Version: ' + esc(p.version || 'N/A') + '</div>' +
              '<div class="prov-caps">' + (caps || '<span class="cap-pill">standard</span>') + '</div>' +
            '</div>';
          }).join('');
        } else {
          provHtml = '<div style="color: var(--text-muted);">No provider descriptors registered.</div>';
        }
        document.getElementById('stat-providers').innerText = provCount;
        document.getElementById('providers-grid').innerHTML = provHtml;

        document.getElementById('last-updated').innerText = 'Updated: ' + new Date().toLocaleTimeString();
      } catch (e) {
        console.error('Fetch error:', e);
      }
    }

    fetchDash();
    setInterval(fetchDash, 3000);
  </script>
</body>
</html>`, html.EscapeString(proj), html.EscapeString(ver))
}
