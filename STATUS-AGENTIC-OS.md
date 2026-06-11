# Agentic OS Roadmap — Status

**As of 2026-05-19.** Honest snapshot of how the 20-item Mission Profiles
roadmap actually landed, based on the cross-model audit in
[`AUDIT-AGENTIC-OS.md`](./AUDIT-AGENTIC-OS.md) (Claude implemented; Gemini
audited).

| | Count |
|---|---|
| ✅ Fully complete (10/10) | 5 |
| 🟡 Substantive (8–9/10) | 10 |
| 🟡 Partial (6–7/10) | 5 |
| ❌ Failed audit | 0 |
| **Total** | **20** |

---

## Per-item table

| # | Title | Score | Verdict |
|---|---|---|---|
| 1 | MissionProfile schema + loader | 10/10 | ✅ |
| 2 | Wire `--mode` flag + dev profile | 6/10 | 🟡 partial — profile enforcement only in `run` |
| 3 | MCP client + Gmail bridge | 8/10 | 🟡 — Gemini `CapMCP` not actually wired; bridge config-only |
| 4 | Publishing MCP + `ORG_STATE.md` | 9/10 | 🟡 — missing HITL triggers in ops profile |
| 5 | Audit mode kernel + Slither/Aderyn | 7/10 | 🟡 — `internal/audit/` is dead code, profile example-only |
| 6 | Context switching: threads + state.json | 10/10 | ✅ |
| 7 | Capability gates | 8/10 | 🟡 — Sentinel not in resume/improve |
| 8 | Research mode + Tavily/WebFetch | 10/10 | ✅ |
| 9 | Multi-agent handoff: dev → audit → human | 10/10 | ✅ |
| 10 | `uta mode list` + `uta dash` (mode-aware) | 9/10 | 🟡 — hardcoded mode panes in dash.go |
| 11 | Cross-mode memory + InstitutionalMemory | 8/10 | 🟡 — duplicate facts in Reflector; missing injection |
| 12 | Automated retrospectives | 7/10 | 🟡 — lookback not enforced; filename collision; gated by success |
| 13 | Vibe steering per mode | 10/10 | ✅ |
| 14 | Token + dollar budget hard caps | 6/10 | 🟡 — only enforced in `run`; missing in resume/improve/audit |
| 15 | Shadow sandbox | 8/10 | 🟡 — sandbox copy unclear; some dead code |
| 16 | Human-in-the-loop signature gates | 9/10 | 🟡 — HITL not enforced in resume turns |
| 17 | Latency profiling | 7/10 | 🟡 — Reflector phases missing; CPU double-counting |
| 18 | Inter-agent whiteboard | 8/10 | 🟡 — tools unreachable from non-MCP providers |
| 19 | Mode-specific dashboards | 9/10 | 🟡 — hardcoded mode names violate generic rule |
| 20 | Self-audit workflow | 8/10 | 🟡 — E2E test assertions incomplete |

---

## Themes (clusters of related gaps)

### A. Policy/budget/sentinel/HITL is wired in `run` but not in `resume` / `improve` / `audit` (≈14 of 39 gaps)

The biggest single architectural gap. The `uta run` command builds a full
"orchestration preamble" — loads the active profile, applies its policies,
wires the Sentinel watcher, configures HITL triggers, sets up the budget.
The other entry points (`resume`, `improve`, `audit`) each do a partial
subset. Touches items 2, 7, 14, 16, and indirectly 17.

**Fix shape:** extract `applyOrchestrationPreamble(app, req, mode)` and
call it identically from every entry point. One refactor closes the
pattern.

### B. Dead-code libraries (items 5, 15, parts of 18)

`internal/audit/`, `internal/shadow/`, and parts of `internal/whiteboard/`
exist as testable packages but have no caller in the run loop or CLI
surface. They're reachable in unit tests but not from any user-facing
command.

**Fix shape:** per-package "wire or delete" decision. Some (whiteboard
tools) need to be exposed as worker-callable tools. Others (parts of the
shadow CLI) might be over-built for now and should be trimmed.

### C. Hardcoded mode names violate the generic-first rule (items 10, 19)

`internal/cli/dash.go` has `["dev", "ops", "audit", "research"]` baked in
as the mode-pane list. Should derive from loaded profiles instead.

**Fix shape:** 30-min refactor — iterate over `app.LoadedProfiles()` and
render a pane per mode that has data.

### D. MCP integration depth (item 3)

The MCP client itself is solid. The bridge between probed MCP tools and
the worker provider is incomplete: Gemini advertises `CapMCP` but doesn't
actually pass `MCPConfigPath`, and the bridge has no proxying path for
non-native-MCP providers.

**Fix shape:** real architectural work. Probably its own focused cycle,
not a sweep.

### E. Reflector polish (items 11, 12, 17)

Edge cases in deduplication (item 11), retrospective scheduling (item 12),
and latency phase accounting (item 17). Each is small but distinct.

---

## What's shipped right now

The 5 fully-complete items represent a real, usable subset:

- **Mission Profiles** (items 1, 6, 9) — profile schema, named threads,
  multi-agent handoffs.
- **Research mode** (item 8) — `RESEARCH_DATABASE.md` with citations.
- **Vibe steering** (item 13) — per-mode prompt overrides.

This is enough to demo "uta has modes" credibly. The remaining 15 items
are reachable as Go libraries with passing tests; they just aren't
wired into all user-facing commands consistently.

---

## What we're closing next

**Theme A only** (the integration preamble). This single refactor lifts:

- Item 2 from 6 → 9 (closes 3 of 4 gaps)
- Item 7 from 8 → 10 (closes both gaps)
- Item 14 from 6 → 9 (closes 3 of 5 gaps)
- Item 16 from 9 → 10 (closes the resume gap)
- Item 17 partial credit (Reflector phases become enumerable once the
  preamble is uniform)

Projected after Theme A: **8 complete, 7 substantive, 3 partial, 2 below
7/10**. Better-than-half is plumb finished.

---

## What we're deferring (and why)

| Theme | Why deferred |
|---|---|
| B (dead-code libraries) | Each needs a per-package decision — not a refactor |
| C (hardcoded mode names) | 30 min when somebody touches `dash.go` next |
| D (MCP provider bridge) | Real architectural work — own focused cycle |
| E (Reflector polish) | Small fixes; can land opportunistically |

The honest meta-point: 5/20 fully complete after a 2-day autonomous build
is a real result. The 15 partials each have a precise gap list — qualitatively
different from "I'm not sure what's done."

The verify gate (`go test && go build`) measures isolated correctness.
Integration correctness needs a second eye, which is what this audit
provided. The signal cost (≈100 min of Gemini, ≈$0.50 in API) was worth
more than the prior 25-iter autonomous refinement round, by a wide margin.

---

## Source documents

- [`AUDIT-AGENTIC-OS.md`](./AUDIT-AGENTIC-OS.md) — raw per-item audit
  with all 39 gap descriptions.
- `.uta/audit-raw/item-*.raw.txt` — raw Gemini responses (gitignored).
- `scripts/audit-agentic-os.sh` — the audit script used (gitignored;
  dev-only).
