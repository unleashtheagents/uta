#!/usr/bin/env bash
# scripts/seed-agentic-os.sh — seed the idea board with the 20-iteration
# "Agentic OS" roadmap (Mission Profiles / multi-mode evolution).
#
# Each idea carries:
#   - severity=high so the improve loop picks them off the top (sequence
#     within same-severity is insertion order with the default picker;
#     don't mix in other high-severity ideas mid-run).
#   - tag agentic-os-roadmap, plus a roadmap-NN tag for filtering.
#   - a body with concrete file paths, acceptance criteria, and out-of-scope
#     warnings so the worker stays inside one phase.
#
# Usage:
#   ./scripts/seed-agentic-os.sh             # add all 20 if not present
#   ./scripts/seed-agentic-os.sh --reset     # reject all existing
#                                              agentic-os-roadmap ideas
#                                              first, then add fresh
#   ./scripts/seed-agentic-os.sh --dry-run   # print titles only
#
# After seeding, run the loop with autopilot disabled for new gathers:
#   GATHER_WHEN_EMPTY=0 BUDGET=72h MAX_ITER=20 PER_IDEA=2h \
#     VERIFY='go test ./... && go build ./...' \
#     ./scripts/self-improve.sh
set -euo pipefail

cd "$(dirname "$0")/.."

DRY_RUN=0
RESET=0
for arg in "$@"; do
  case "$arg" in
    --dry-run) DRY_RUN=1 ;;
    --reset)   RESET=1 ;;
    -h|--help)
      sed -n '1,/^set -euo/p' "$0" | sed -n 's/^# \{0,1\}//p'
      exit 0 ;;
    *) echo "unknown arg: $arg" >&2; exit 1 ;;
  esac
done

if [ "$RESET" = "1" ] && [ "$DRY_RUN" = "0" ]; then
  echo "==> rejecting existing roadmap ideas (titles matching '[NN/20] ...')"
  if ! command -v jq >/dev/null 2>&1; then
    echo "abort: --reset requires jq (brew install jq)" >&2
    exit 1
  fi
  uta ideas list --status proposed --limit 200 --json 2>/dev/null \
    | jq -r '.[] | select(.Title | test("^\\[[0-9]+/20\\]")) | .ID' \
    | while read -r id; do
        [ -z "$id" ] && continue
        echo "  rejecting $id"
        uta ideas reject "$id" --note "reseed" >/dev/null
      done
fi

add_idea() {
  local n="$1" title="$2" body="$3"
  local tag="roadmap-$(printf '%02d' "$n")"
  if [ "$DRY_RUN" = "1" ]; then
    printf "[%02d] %s\n" "$n" "$title"
    return
  fi
  printf "  adding idea %02d: %s\n" "$n" "$title"
  uta ideas add \
    --title "[$n/20] $title" \
    --body "$body" \
    --severity high \
    --tag agentic-os-roadmap \
    --tag "$tag" >/dev/null
}

# ────────────────────────────────────────────────────────────────────────────
# PHASE 1 — Mission Profile Foundation (iters 1–10)
# ────────────────────────────────────────────────────────────────────────────

add_idea 1 "MissionProfile schema + loader" "$(cat <<'EOF'
Design and ship the MissionProfile system that turns uta from one-shape-fits-all into modal.

Deliverables:
- internal/profile/profile.go: MissionProfile struct (Name, Description,
  Personas []string, AllowedTools []string, DeniedTools []string,
  Env map[string]string, Policies struct{HITLSeverity string;
  TokenBudget int; PerCallBudget int}).
- internal/profile/loader.go: scan ~/.uta/profiles/*.yaml and
  <project>/.uta/profiles/*.yaml; project-local wins.
- internal/profile/profile_test.go: parse + validate + override-chain tests.
- internal/cli/mode.go: `uta mode list` and `uta mode show <name>`.
- examples/profiles/default.yaml — current behavior expressed as a profile.

Acceptance:
- `uta mode list` shows at least the embedded "default" profile.
- `go test ./internal/profile/...` passes.
- A YAML profile with unknown fields fails loud (KnownFields=true).
- Out-of-scope: --mode flag wiring (lands in iter 2). Persona toolkits
  (iter 7). Don't touch supervisor.go or run.go.
EOF
)"

add_idea 2 "Wire --mode flag + dev profile" "$(cat <<'EOF'
Plumb MissionProfile through the run path. Ship the 'dev' profile as the
first real one.

Deliverables:
- internal/cli/root.go: add persistent --mode <name> flag.
- internal/cli/run.go + improve.go + audit.go: when --mode is set,
  load the named profile, merge its Env into RunRequest.Env, restrict
  PreApproveTools to AllowedTools ∩ user-supplied.
- examples/profiles/dev.yaml: personas=[architect, engineer]; allowed
  tools = Read, Edit, Write, Bash(go *), Bash(git status), Bash(git diff);
  HITLSeverity=high.
- engine/profile_apply.go: ApplyProfile(req *RunRequest, p *MissionProfile).
- Test: a run with --mode dev that tries to spawn `Bash(curl *)` is
  rejected before the provider sees it.

Acceptance:
- `uta run -g "..." --mode dev -y` works end-to-end with claude.
- `uta run -g "..." --mode nonexistent` errors with a clean message
  and exit code 2.
- Out-of-scope: ops / audit / research modes (later iters). Capability
  gates as a separate enforcement layer (iter 7) — for now, AllowedTools
  is just a pre-approve allow-list.
EOF
)"

add_idea 3 "MCP client + Gmail bridge (ops mode, read-only)" "$(cat <<'EOF'
Add the ability for a uta worker to call an external MCP server. Ship
Gmail as the proof-of-life integration.

Deliverables:
- internal/mcp/client.go: a minimal MCP client that speaks JSON-RPC over
  stdio to a launched server binary; exposes tools/list and tools/call.
- internal/profile/mcp.go: ProfileMCPServer{Name, Command, Args, Env}.
  MissionProfile gains MCPServers []ProfileMCPServer.
- engine/mcp_tool_bridge.go: when a worker is invoked, expose loaded MCP
  tools through the provider's tool layer (claude: pass via --allowedTools;
  declarative: surface as additional callable tools). This is the
  trickiest piece — keep it claude-only in this iter, declarative in a
  later one.
- examples/profiles/ops.yaml: personas=[chief-of-staff,
  comms-agent]; mcp_servers: [{name: gmail, command: npx, args:
  ['-y','@modelcontextprotocol/server-gmail'], env: {GMAIL_OAUTH_PATH:
  '${UTA_OPS_GMAIL_OAUTH}'}}]; HITLSeverity=medium.
- internal/mcp/client_test.go: against a fixture stub server in tests/.

Acceptance:
- `uta run -g "Summarize my 5 most recent emails." --mode ops -y` returns
  a summary when a real Gmail MCP server is configured.
- Without OAuth: clean error, no panic.
- Out-of-scope: WordPress (iter 4), Slack, Calendar. Multi-server
  composition (iter 7's capability gates handle that).
EOF
)"

add_idea 4 "Publishing MCP + ORG_STATE.md (ops write-side)" "$(cat <<'EOF'
Close the read-write loop for ops mode. The agent should be able to take
an email summary and publish a news entry.

Deliverables:
- examples/profiles/ops.yaml: extend with a wordpress (or ghost) MCP
  server entry. Make the choice configurable via UTA_OPS_CMS env.
- internal/context/org_state.go: ORG_STATE.md reader/writer scoped to the
  project context dir. Schema: yaml frontmatter (last_updated, source_emails,
  pending_decisions) + free-form markdown body.
- engine/synth.go: no change — re-use existing synthesis. The persona
  prompt does the rest.
- examples/personas/comms-agent.yaml: charter says
  "draft, never publish without explicit approval".
- Smoke test docs in docs/ops-mode.md (markdown is fine, no automation).

Acceptance:
- `uta run -g "Read this week's investor emails. Update ORG_STATE.md
  with new commitments. Draft a news post about Q3 goals." --mode ops`
  produces:
  - ORG_STATE.md updated with structured commitments,
  - a draft post in the WordPress site (or whichever CMS),
  - exit 0 with HITL pause if HITLSeverity is configured strict.
- Out-of-scope: slack, calendar (later). The HITL pause itself is
  scaffolded here but implemented in iter 16.
EOF
)"

add_idea 5 "Audit mode kernel + Slither/Aderyn bridge" "$(cat <<'EOF'
Wrap the existing Reflector strategy into a first-class 'audit' profile
with smart-contract-specific tooling.

Deliverables:
- examples/profiles/audit.yaml: personas=[trail-of-bits, openzeppelin-style,
  formal-verification]; mcp_servers (or tool adapters) for slither + aderyn;
  AllowedTools includes Read + Bash(slither *) + Bash(aderyn *).
- internal/engine/tools/aderyn.go: new tool adapter modeled on the existing
  slither adapter (engine/tools.go already has the pattern).
- internal/audit/contract_audit.go: orchestrator-side helper that, given a
  contract path, builds a ReflectorRequest with the audit personas + tools.
- New CLI: `uta audit --mode audit <path>` (the --mode here is redundant
  but lets users override personas without rewriting flags).
- Tests with a vulnerable fixture contract in tests/fixtures/contracts/.

Acceptance:
- `uta audit ./Token.sol --mode audit` produces a findings.json with
  at least one HIGH severity finding from at least one source.
- SARIF output remains valid (iter 0.4.1 invariant).
- Out-of-scope: fuzzers (Echidna, Foundry invariant testing) — iter 7
  can add them as capability-gated tools. Audit on non-Solidity code
  (Python/Go SAST) is left for a future profile.
EOF
)"

add_idea 6 "Context switching: named threads + state.json" "$(cat <<'EOF'
Let one user juggle dev and ops work without state collisions.

Deliverables:
- internal/state/threads.go: Thread{ID, Name, Mode, ActiveSessionID,
  ContextRefs []string, CreatedAt, LastUsedAt}.
- .uta/state.json (project) and ~/.uta/state.json (global) hold the
  thread index. Atomic writes via paths.WriteFileAtomic (already exists
  as of v0.9.0).
- CLI: `uta thread new <name> --mode <mode>`, `uta thread switch <name|id>`,
  `uta thread list`, `uta thread current`.
- Every `uta run` / `uta improve` writes the resulting session_id into
  the active thread's ActiveSessionID.
- `uta resume` with no args resumes the active thread's last session.

Acceptance:
- Create two threads (dev-feature-x, ops-monthly-recap). Run a goal in
  each. `uta thread list` shows both with the right Mode and last-used
  timestamps. `uta resume` from inside thread dev-feature-x picks up
  the dev session.
- Out-of-scope: cross-thread memory sharing (iter 11). Visual indicator
  in progress renderer (iter 19's dash).
EOF
)"

add_idea 7 "Capability gates: tool-level enforcement" "$(cat <<'EOF'
Replace 'AllowedTools is just a pre-approve list' with real enforcement.

Deliverables:
- internal/engine/capability/gate.go: middleware that wraps a provider's
  tool-call channel. Each tool invocation is matched against the active
  profile's AllowedTools + DeniedTools (DeniedTools wins on collision).
- Provider integration: claude.go and gemini.go forward the gate's
  decision; the declarative provider (yaml_provider.go) gets the same
  hook.
- Trajectory event: capability_gate_denied (new kind in trajectory/event.go,
  surfaced by progress.go and sentinel.go).
- Sentinel rule: 3 denials within 10 events → warn alert (agent is
  probing edges of its sandbox).

Acceptance:
- A 'dev' profile that denies Bash(curl *) blocks a claude tool call
  for `curl https://...` even when claude was invoked outside the
  pre-approve flag context.
- Trajectory shows the denial; the worker either retries another
  approach or fails cleanly.
- Out-of-scope: per-call argument fingerprinting (e.g. distinguishing
  `git status` from `git push --force`). The string-match against the
  Allow/Deny patterns is the v1.
EOF
)"

add_idea 8 "Research mode + Tavily/WebFetch + RESEARCH_DATABASE.md" "$(cat <<'EOF'
Add a profile aimed at long-form synthesis from external sources.

Deliverables:
- examples/profiles/research.yaml: personas=[deep-researcher,
  citation-curator]; mcp_servers: tavily (or perplexity) for search,
  web-fetch for raw page reads; AllowedTools strict (no Edit, no Write
  outside the context dir).
- internal/context/research_db.go: structured writer for
  RESEARCH_DATABASE.md (frontmatter: queries, sources, claims with
  citations).
- examples/personas/citation-curator.yaml: charter mandates a Sources:
  section with URLs + retrieval timestamps for every claim.

Acceptance:
- `uta run -g "What are the current best practices for MCP server
  authentication?" --mode research` produces a RESEARCH_DATABASE.md
  entry with ≥3 sources, each with a URL and a date.
- Out-of-scope: PDF parsing (later). Long-running monitoring of a
  source (different mode entirely — would be 'monitor').
EOF
)"

add_idea 9 "Multi-agent handoff: dev → audit → human" "$(cat <<'EOF'
Allow chained mode invocations so a dev run automatically triggers an
audit before declaring victory.

Deliverables:
- internal/profile/handoff.go: Profile gains OnComplete []Handoff
  field. Handoff{TargetMode, Condition, PromptTemplate}.
- engine/run.go: after a normal run completes successfully, evaluate
  OnComplete handoffs and chain a new run with the next mode.
- examples/profiles/dev.yaml: on_complete: [{target_mode: audit,
  condition: 'files_changed > 0 && contains_any: [*.sol, *.go]',
  prompt: 'Audit the diff from session {{prior_session_id}}.'}]
- Trajectory: handoff_started, handoff_completed.

Acceptance:
- A dev run that touches a .sol file automatically triggers an audit
  run on the diff. Both sessions are linked via meta_json.
  resumed_from-style fields.
- Cancelling the handoff (Ctrl-C) leaves the original run completed
  and the handoff marked cancelled.
- Out-of-scope: HITL between handoffs (iter 16). Loop detection on
  handoff chains (the sentinel already counts events globally).
EOF
)"

add_idea 10 "uta mode list + uta dash (mode-aware)" "$(cat <<'EOF'
Polish the discovery surface so users see which modes are installed,
recently used, and what they cost.

Deliverables:
- internal/cli/mode.go: `uta mode list --json` enumerates profiles with:
  name, description, persona count, mcp_server count, last_used_at,
  total_sessions, total_subtask_seconds.
- internal/cli/dash.go: new top-level command. ASCII dashboard rendered
  to stdout (NOT a TUI loop — single render, scriptable). Sections:
  Active threads, Recent sessions per mode, Sentinel alerts last 24h,
  Idea board stats per mode (group by 'mode' tag).
- internal/store: add session.mode_name column via migration 0005.

Acceptance:
- `uta mode list` works in a fresh repo (shows just 'default').
- `uta dash` works in a project with ≥1 session run; doesn't blow up
  on empty DBs.
- Out-of-scope: a real TUI (separate effort). Live-updating panes
  (later — for now, you re-run `uta dash`).
EOF
)"

# ────────────────────────────────────────────────────────────────────────────
# PHASE 2 — Refinement & "The Soul" (iters 11–20)
# ────────────────────────────────────────────────────────────────────────────

add_idea 11 "Cross-mode memory + InstitutionalMemory store" "$(cat <<'EOF'
Findings discovered in one mode should inform behavior in others.

Deliverables:
- internal/memory/store.go: InstitutionalMemory backed by SQLite (new
  table: institutional_facts{id, source_mode, source_session_id, kind,
  body, created_at, tags}).
- Hooks: after each session, an opt-in "consolidator" step (mode profile
  controls opt-in) writes salient takeaways. After every audit finding
  of severity HIGH, an institutional fact of kind="lint_rule" is also
  written.
- Read-side: planner prompts get a `## Relevant prior facts` section
  injected from the store, top-N by tag overlap with the goal.

Acceptance:
- An audit run that finds a reentrancy bug in dev mode results in a
  fact-row that a subsequent dev-mode run on a related file picks up
  and references in its plan.
- Stats: `uta memory stats` shows fact counts by source_mode.
- Out-of-scope: vector search / embeddings — tag overlap is the v1
  retrieval signal. LLM-based fact summarization (later).
EOF
)"

add_idea 12 "Automated per-mode retrospectives" "$(cat <<'EOF'
Every N sessions in a mode, generate a LESSONS_LEARNED.md for that mode.

Deliverables:
- internal/improve/retrospective.go: schedule check after each `uta run`
  / `uta improve` iteration. When session_count_for_mode % 5 == 0,
  spawn a synthesis run that reads the last 5 trajectories and writes
  <project>/.uta/context/retrospectives/<mode>-<YYYYMMDD>.md.
- The retrospective worker is the same provider as the mode's default;
  the prompt is templated per-mode (examples/profiles/<mode>.yaml gains
  retrospective_prompt: <text>).
- Trajectory: retrospective_started, retrospective_completed.

Acceptance:
- After 5 dev-mode sessions, a retrospective markdown is written and
  surfaces in `uta dash`.
- Disabling retrospectives via profile (retrospective_every: 0) skips
  the step entirely.
- Out-of-scope: cross-mode retrospectives. Auto-applying lessons as
  feedback memory (the lessons file is human-curated input).
EOF
)"

add_idea 13 "Vibe steering per mode" "$(cat <<'EOF'
Let the user nudge a mode's behavior between runs without editing YAML.

Deliverables:
- internal/cli/vibe.go: `uta vibe <mode> "<directive>"` appends a vibe
  entry (timestamped) to <project>/.uta/profiles/<mode>.vibe.md.
- Loader: when a mode is loaded, any matching .vibe.md is appended to
  every persona prompt as a `## Current vibe` section.
- `uta vibe <mode> --clear` removes it. `uta vibe <mode> --show`
  prints current vibe.

Acceptance:
- `uta vibe audit "be more aggressive — flag anything that smells off,
  not just confirmed bugs"` changes the next audit run's findings
  density visibly (more LOW/INFO findings).
- Out-of-scope: vibe drift detection (sentinel rule that warns if vibe
  text is producing erratic results).
EOF
)"

add_idea 14 "Token + dollar budget hard caps" "$(cat <<'EOF'
Stop autonomous ops runs from racking up unbounded API spend.

Deliverables:
- internal/budget/budget.go: Budget{MaxTokens, MaxUSDCents,
  PerCallMaxTokens}. Each provider's RunResult gains TokensIn / TokensOut /
  ApproxUSDCents (best-effort — claude streams usage events; gemini
  may need post-hoc estimation).
- internal/engine/supervisor.go: before each provider call, check budget;
  on exceed, abort with ErrBudgetExceeded (new sentinel error).
- MissionProfile.Policies.TokenBudget and .DollarBudgetCents enforced
  at the supervisor level.
- Trajectory: budget_warning (80%), budget_exhausted (100%).

Acceptance:
- A run with MaxUSDCents=10 against an expensive prompt aborts with a
  clean error and a session marked status=budget_exhausted.
- The existing wall-clock Budget (sentinel.BudgetWallClock) keeps
  working unchanged.
- Out-of-scope: per-tool budgets (e.g. "Tavily can spend up to $1").
EOF
)"

add_idea 15 "Shadow sandbox: regression-test prompt changes" "$(cat <<'EOF'
When a vibe / profile / persona changes, re-run it against historical
trajectories to make sure outputs haven't regressed.

Deliverables:
- internal/shadow/shadow.go: Shadow{SourceSessionID, ProfileBefore,
  ProfileAfter}.Run() replays the original goal against the new profile
  in a sandboxed copy of the project and produces a diff report.
- CLI: `uta shadow run --mode <name> --since 7d` replays every session
  in that mode from the last week.
- Output: shadow report markdown showing per-session deltas (token
  count, output diff, exit status).

Acceptance:
- After editing examples/profiles/audit.yaml, `uta shadow run --mode
  audit --since 30d` produces a report flagging any session whose
  output changes by > N tokens (configurable).
- Out-of-scope: auto-rollback on regression (left as a user decision).
  Parallel shadow runs (works fine sequentially for now).
EOF
)"

add_idea 16 "Human-in-the-loop signature gates" "$(cat <<'EOF'
For high-stakes actions, pause and prompt the human.

Deliverables:
- internal/hitl/gate.go: HITLGate{Action, Severity, PromptText, Approver}.
  Synchronous prompt to stderr when uta is attached to a TTY; async via
  a file-drop (.uta/hitl/<session>/pending.json) otherwise.
- Triggers (all configurable per profile):
  - any Bash(* push *), Bash(* deploy *), Bash(forge create *)
  - ops mode: any publish / send-email tool call
  - audit mode: any --fix attempt
  - improve mode: when the current iteration crosses TokenBudget *
    threshold
- A `uta hitl approve <session> --action <id>` command for async.

Acceptance:
- `uta run -g "publish today's draft" --mode ops` pauses, prints the
  draft, and waits for [y/N] on the TTY. Pressing 'n' aborts with a
  clean trajectory entry hitl_denied.
- The async flow works when stdin is /dev/null: a pending.json appears;
  approving it unblocks the run.
- Out-of-scope: multi-signature (two humans approve) — single-approver
  is the v1.
EOF
)"

add_idea 17 "Latency profiling: thinking vs tool time" "$(cat <<'EOF'
Identify which step in a run is the bottleneck so it can be optimized.

Deliverables:
- internal/profile_perf/profile.go: per-session breakdown of time in
  planner / per-subtask provider / synthesis / sentinel / tool calls.
- Uses trajectory event timestamps (already present); no new
  instrumentation needed in providers.
- CLI: `uta perf <session-id>` renders a flame-graph-style ascii summary.
  `uta perf --mode dev --since 7d` aggregates across sessions.

Acceptance:
- For a run with 5 subtasks and 3 gate executions, `uta perf <id>`
  shows wall-clock vs CPU-equivalent split, plus a per-tool breakdown.
- Out-of-scope: real flame graphs (PNG/SVG). Live profiling during a
  run (post-hoc only for now).
EOF
)"

add_idea 18 "Inter-agent whiteboard" "$(cat <<'EOF'
A shared JSON state that active threads / modes can write to and read
from, so dev and ops can leave notes for each other.

Deliverables:
- internal/whiteboard/board.go: a per-project SQLite-backed key-value
  store with append-only history. Schema: whiteboard{key, value_json,
  author_mode, author_session, ts}.
- Tool surface: every mode gets uta_whiteboard_set / _get / _list
  (capability-gated; ops mode can write, dev mode can read by default).
- Trajectory events: whiteboard_set, whiteboard_get.

Acceptance:
- An ops-mode run writes a "blocker" note for dev. The next dev-mode
  run sees it in its planner context (via the InstitutionalMemory
  injector from iter 11) and reflects it in its plan.
- Out-of-scope: real-time pub/sub (poll-on-load is fine).
EOF
)"

add_idea 19 "Mode-specific dashboards" "$(cat <<'EOF'
Refine `uta dash` from iter 10 with per-mode panes.

Deliverables:
- Per-mode renderer functions in internal/cli/dash.go:
  - dashDev: burndown of ideas done vs proposed; test pass-rate.
  - dashOps: emails processed, drafts published, ORG_STATE.md
    last-updated.
  - dashAudit: findings heatmap by severity + file.
  - dashResearch: source coverage, RESEARCH_DATABASE.md size.
- `uta dash --mode <name>` shows only that pane.
- `uta dash --json` for tooling integration (e.g. status bar widgets).

Acceptance:
- Each pane renders something useful even when the data is sparse.
- Out-of-scope: GROWTH / RECRUIT / COMPLY / ARCHIVE modes — those come
  as separate iterations (post-roadmap; see the feasible-modes list).
EOF
)"

add_idea 20 "Self-audit: uta audits its own 20-iter progress" "$(cat <<'EOF'
The unleashed final step: have uta perform a Reflector audit of its
own evolution and propose the next 20-iteration roadmap.

Deliverables:
- A workflow YAML examples/workflows/self-audit.yaml that:
  - Reads CHANGELOG.md / git log for v0.9.x → v0.10.x range.
  - Reads each mode profile + persona file.
  - Reads recent retrospectives (output of iter 12).
  - Runs auditor critics (architecture, completeness, regression-risk).
  - Produces ROADMAP-V2.md with 20 new proposed iterations.
- One smoke test in tests/ that runs the workflow in --dry-run mode
  and asserts ROADMAP-V2.md is produced and non-empty.

Acceptance:
- `uta run -f examples/workflows/self-audit.yaml --mode audit` produces
  ROADMAP-V2.md.
- The findings.json from this run flags at least one cross-mode gap
  (e.g. "no mode has implemented X yet").
- Out-of-scope: actually adopting the proposed roadmap (that's a human
  decision).
EOF
)"

# ────────────────────────────────────────────────────────────────────────────

if [ "$DRY_RUN" = "1" ]; then
  echo "(dry-run — no ideas inserted)"
  exit 0
fi

echo
echo "==> seeded 20 ideas. Inspect with:"
echo "    uta ideas list --severity high --limit 30 | head"
echo
echo "==> next: launch the autopilot (72h budget, no auto-gather):"
echo "    GATHER_WHEN_EMPTY=0 BUDGET=72h MAX_ITER=20 PER_IDEA=2h \\"
echo "      VERIFY='go test ./... && go build ./...' \\"
echo "      ./scripts/self-improve.sh"
