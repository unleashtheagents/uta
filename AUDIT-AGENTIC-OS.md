# Agentic OS Roadmap — Codebase Audit

Generated 2026-05-19T16:40:07Z
Auditor: `gemini`  ·  Items audited: 1 2 3 4 5 6 7 8 9 10 11 12 13 14 15 16 17 18 19 20

Each section evaluates the **current state of the codebase** against the
REFINE idea's full spec body (which embeds the original spec + the prior
audit's gap list).


## [1/20] REFINE: MissionProfile schema + loader

**Verdict**: ✅ complete  ·  **Score**: 10/10  ·  **Gaps**: 0

## [2/20] REFINE: Wire --mode flag + dev profile

**Verdict**: 🟡 partial  ·  **Score**: 9/10  ·  **Gaps**: 1

### Gaps

- **[2/20] improve loop ignores memory and handoff policies**
    internal/improve/loop.go: The LoopRequest struct and ApplyProfile helper are missing the MemoryConsolidate, MemoryTopK, and OnComplete fields, preventing MissionProfile policies for institutional memory and chained handoffs from being respected during self-improvement iterations.

## [3/20] REFINE: MCP client + Gmail bridge (ops mode, read-only)

**Verdict**: 🟡 partial  ·  **Score**: 9/10  ·  **Gaps**: 1

### Gaps

- **[3/20] Gemini provider advertises CapMCP but ignores MCPConfigPath**
    The 'gemini' provider in internal/provider/builtin/gemini.go includes provider.CapMCP in its Capabilities list during detection, but the runHeadless implementation fails to pass opts.MCPConfigPath as a flag (e.g., --mcp-config) to the gemini CLI. While the 'claude' and 'declarative' providers correctly wire this path, gemini users cannot currently utilize MCP servers attached to their MissionProfile.

## [4/20] REFINE: Publishing MCP + ORG_STATE.md (ops write-side)

**Verdict**: ✅ complete  ·  **Score**: 10/10  ·  **Gaps**: 0

## [5/20] REFINE: Audit mode kernel + Slither/Aderyn bridge

**Verdict**: 🟡 partial  ·  **Score**: 8/10  ·  **Gaps**: 2

### Gaps

- **[5/20] Slither adapter file location**
    internal/engine/slither.go is missing; the adapter logic is currently merged into internal/engine/tools.go, which is inconsistent with the standalone internal/engine/aderyn.go.
- **[5/20] Missing vulnerable fixture contracts**
    No vulnerable fixture contracts (e.g., tests/fixtures/contracts/reentrancy.sol) are present to verify the tool adapters as required by the spec.

## [6/20] REFINE: Context switching: named threads + state.json

**Verdict**: ✅ complete  ·  **Score**: 10/10  ·  **Gaps**: 0

## [7/20] REFINE: Capability gates: tool-level enforcement

**Verdict**: 🟡 partial  ·  **Score**: 8/10  ·  **Gaps**: 1

### Gaps

- **[7/20] Sentinel not wired in resume command**
    The Sentinel watcher, which enforces the 3-in-10 capability_gate_denied rule, is wired in the 'run', 'improve', and 'audit' commands, but is completely missing from 'internal/cli/resume.go'. As a result, the capability gate sentinel alert will never fire during a resumed session.

## [8/20] REFINE: Research mode + Tavily/WebFetch + RESEARCH_DATABASE.md

**Verdict**: 🟡 partial  ·  **Score**: 9/10  ·  **Gaps**: 1

### Gaps

- **[8/20] perplexity server**
    examples/profiles/research.yaml wires tavily but omits perplexity alternative mentioned in roadmap.

## [9/20] REFINE: Multi-agent handoff: dev → audit → human

**Verdict**: ✅ complete  ·  **Score**: 10/10  ·  **Gaps**: 0

## [10/20] REFINE: uta mode list + uta dash (mode-aware)

**Verdict**: 🟡 partial  ·  **Score**: 7/10  ·  **Gaps**: 3

### Gaps

- **[10/20] dash --mode filter is exclusive**
    internal/cli/dash.go renders only the deep-dive pane when --mode is set, instead of filtering all dashboard sections (threads, sessions, alerts, ideas) to that mode.
- **[10/20] dash --json missing global sections**
    internal/cli/dash.go:emitDashJSON omits top-level sections like active threads and sentinel alerts, rendering only per-mode panes.
- **[10/20] dash panes are not generic**
    internal/cli/dash.go:panesToRender filters against a hardcoded paneRegistry, preventing deep-dive panes from appearing for user-defined profiles or modes with recorded data.

## [11/20] REFINE: Cross-mode memory + InstitutionalMemory store

**Verdict**: 🟡 partial  ·  **Score**: 8/10  ·  **Gaps**: 2

### Gaps

- **[11/20] Resume missing MemoryConsolidate integration.**
    internal/engine/resume.go (ResumeRequest) and internal/engine/profile_apply.go (ApplyProfileToResume) are missing the field and logic to trigger consolidateRunOutcome, preventing resumed sessions from respecting the profile's memory.consolidate policy.
- **[11/20] Improve missing memory policy integration.**
    internal/improve/loop.go (LoopRequest and ApplyProfile) and internal/engine/profile_apply.go (missing ApplyProfileToLoop helper) do not forward MemoryConsolidate or MemoryTopK from the active MissionProfile, preventing self-improvement iterations from participating in institutional memory.

## [12/20] REFINE: Automated per-mode retrospectives

**Verdict**: 🟡 partial  ·  **Score**: 7/10  ·  **Gaps**: 3

### Gaps

- **[12/20] Synthesis lookback count mismatch**
    internal/improve/retrospective.go: MaybeRetrospective uses req.Every as the limit for RecentSessionsByMode, whereas the SPEC requires synthesizing exactly the 'last 5 trajectories' regardless of the trigger cadence.
- **[12/20] Missing integration in resume and audit commands**
    internal/cli/resume.go and internal/cli/audit.go: These commands do not invoke maybeWriteRetrospective, meaning sessions concluded via resumption or auditing do not contribute to cadence triggers or trigger synthesis.
- **[12/20] Retrospective gated by success in run command**
    internal/cli/run.go: The retrospective check is only performed if runErr == nil, which prevents synthesis from occurring when a failing session hits a cadence boundary, despite having enough session history.

## [13/20] REFINE: Vibe steering per mode

**Verdict**: 🟡 partial  ·  **Score**: 8/10  ·  **Gaps**: 3

### Gaps

- **[13/20] Vibe not loaded for default mode**
    internal/cli/audit.go: applyVibeToCritics is only called when mode is non-nil. Running `uta audit` without --mode should still load vibes from default.vibe.md if present, as 'default' is the implicit active mode.
- **[13/20] Library-side audit loader is not vibe-aware**
    internal/audit/contract_audit.go: BuildReflectorRequest and its helpers do not load or apply vibes to critics, making the library implementation inconsistent with the CLI and the 'Loader' requirement.
- **[13/20] Vibe integration missing from non-audit commands**
    internal/cli/run.go and internal/cli/improve.go: These commands do not support vibes. While they don't use personas today, vibes are intended as a 'per-mode steering knob' and should ideally influence the main goal or subtasks when a mode is active.

## [14/20] REFINE: Token + dollar budget hard caps

**Verdict**: 🟡 partial  ·  **Score**: 7/10  ·  **Gaps**: 1

### Gaps

- **[14/20] Missing budget enforcement in Resume and RunReflector**
    While the resource budgeting logic is fully implemented in internal/budget and enforced in the primary Supervisor.Run path, it is not yet wired into the Resume and RunReflector (audit) entry points. Supervisor.Resume bypasses s.callProvider and lacks budget initialization/accounting, while Supervisor.RunReflector fails to inject the Budget into the context, causing s.callProvider to skip enforcement for critics and revisers. Additionally, Gemini usage reporting relies on heuristic estimation (4 chars per token) and manual environment variable configuration for USD costs, as the Gemini CLI does not yet stream authoritative usage stats.

## [15/20] REFINE: Shadow sandbox: regression-test prompt changes

**Verdict**: 🟡 partial  ·  **Score**: 7/10  ·  **Gaps**: 2

### Gaps

- **[15/20] Sandboxed copy of the project**
    internal/shadow/shadow.go: SandboxWorkdir() returns an empty temp directory instead of a copy of the project files. This violates the SPEC requirement for a 'sandboxed copy' and causes replays of project-dependent goals to fail because the workspace is empty.
- **[15/20] ProfileBefore integration in CLI**
    internal/cli/shadow.go: The shadow run command passes nil for ProfileBefore to shadow.RunMode. This results in the drift report showing '(none)' for the baseline profile, even though the sessions being replayed belong to a known ModeName that could be resolved and used for comparison.

## [16/20] REFINE: Human-in-the-loop signature gates

**Verdict**: 🟡 partial  ·  **Score**: 8/10  ·  **Gaps**: 3

### Gaps

- **[16/20] HITL not enforced in improve loop**
    internal/improve/loop.go and internal/cli/improve.go miss the HITL approver wiring. LoopRequest lacks a HITL field, and executeIdea does not pass req.HITL to the supervisor's Run call, leaving triggers inert during autonomous improvement.
- **[16/20] HITL not enforced in audit (Reflector) mode**
    internal/engine/reflector.go and internal/cli/audit.go do not wire HITL. ReflectorRequest lacks a HITL field, and RunReflector does not initialize hitlState on the context, so callProvider falls back to an inert gate.
- **[16/20] HITL not enforced in handoff chains**
    internal/cli/run.go misses forwarding the HITL approver in executeHandoffChain. Chained runs do not inherit the base request's HITL approver, meaning follow-up modes (e.g. dev -> audit) lose signature protection.

## [17/20] REFINE: Latency profiling: thinking vs tool time

**Verdict**: 🟡 partial  ·  **Score**: 9/10  ·  **Gaps**: 1

### Gaps

- **[17/20] CLI --since flag does not support day units (e.g. "7d")**
    internal/cli/perf.go uses standard time.DurationVar which does not support the 'd' unit specified in the example 'uta perf --since 7d'. Users must currently use hours (e.g. '168h').

## [18/20] REFINE: Inter-agent whiteboard

**Verdict**: 🟡 partial  ·  **Score**: 8/10  ·  **Gaps**: 2

### Gaps

- **[18/20] Planner injection heading mismatch**
    Whiteboard entries are injected into the planner prompt under a '## Shared whiteboard' section in internal/engine/whiteboard_hooks.go, whereas the SPEC expected '## Inter-agent notes'.
- **[18/20] Tool definition location**
    The uta_whiteboard_* tools are defined as MCP tools in internal/cli/serve.go rather than internal/engine/tools.go. While they are correctly capability-gated via the supervisor's event drainer, their location differs from the SPEC's primary suggestion.

## [19/20] REFINE: Mode-specific dashboards

**Verdict**: 🟡 partial  ·  **Score**: 7/10  ·  **Gaps**: 2

### Gaps

- **[19/20] Mode names hardcoded in paneRegistry and builders**
    internal/cli/dash.go:45-62 hardcodes names in paneRegistry. buildDevPane, buildOpsPane, etc. hardcode names when reading stats and matching ideas. This prevents custom-named profiles from using specialized panes.
- **[19/20] countOpsDrafts hardcodes 'ops' mode name**
    internal/cli/dash.go:792 hardcodes s.mode_name = 'ops' in the SQL query, breaking draft counting for custom-named ops profiles.

## [20/20] REFINE: Self-audit: uta audits its own roadmap progress

**Verdict**: ✅ complete  ·  **Score**: 10/10  ·  **Gaps**: 0

---

## Summary

- Total audited:    20
- ✅ complete:       5
- 🟡 partial:        15
- ❌ failed to run:  0
