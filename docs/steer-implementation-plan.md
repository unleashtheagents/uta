# steer — implementation plan

Companion to `steer-language.md` (the RFC) and Paper № 02. Grounded in the
actual repo state as of 2026-07-12 — this is a plan from the middle, not
from zero.

## 0 · Where we actually are (verified)

Done and tested:
- **Front-end** (`internal/steer/`): lexer, parser, AST, static checker.
  v0 subset: `agent fn` (worker / costs / prompt), `mission`
  (budget / context / let / emit / calls). RFC constructs beyond the
  subset (`judge`, `until dry`, `gate human`, `with cap`) parse to a
  legible "not yet implemented" diagnostic — programs written against the
  paper degrade gracefully.
- **Execution** (`internal/engine/mission.go`): `Supervisor.RunMission`
  walks the AST, journals to the normal session/trajectory store. Budget
  exhaustion is a typed event (tested). Mission **resume** replays the
  journal — paid work is never re-paid (P3, committed).
- **`par for`** — structured fan-out (P4, committed).
- **CLI**: `uta mission run` / `uta mission check`.
- **Capability gate** (`internal/engine/capability`): pattern-based
  allow/deny applied to every observed tool invocation — the enforcement
  substrate `with cap` will bind to.
- In flight (uncommitted): `internal/steer/schema.go` — record types →
  JSON Schema at the agent boundary.

Design rule carried from the papers: every milestone must keep the three
invariants — every effect journals; no ambient authority; budget is
enforced, not advisory.

## 1 · Milestones

Each milestone = shippable, tested, demoable with a `.steer` example in
`examples/steer/`, and closes with `uta audit internal/steer --mode audit`.

### M1 · Boundary types (finish what's in flight)
`type` declarations → JSON Schema; agent outputs validated at the
boundary; mismatch → bounded retry → typed `SchemaMismatch` failure that
`retry n on E` can catch.
**Accept:** a mission whose agent returns malformed JSON retries twice,
then fails with a catchable typed error; golden test with a scripted
provider.

### M2 · Judgment — `judge … by … require k of n`
Map to the engine's existing critic/discuss machinery with vote counting.
Verdict schema fixed (`Refuted | Stands` + reason). Runtime refinement
tagging: results carry `@k` metadata in the journal (static checking of
`@k` comes in M5 — runtime truth first, compiler enforcement second).
**Accept:** the Paper № 02 `review_release` judge block runs; `require
2 of 3` demonstrably filters; dead-provider behavior explicitly decided
and tested (RFC Q3 — proposal: a dead refuter shrinks n, never counts as
Stands).

### M3 · Humans and authority — `gate human`, `policy`, `with cap`
`gate human` suspends the mission (journal state = awaiting-signature)
and resumes via the existing `hitl approve` path — same journal, same
session. `policy` binds a MissionProfile; `with cap C from policy P`
raises a capability for exactly one block, enforced through
`engine/capability`.
**Accept:** a mission stops at a gate, survives a process restart,
resumes on approval; a call needing `git.write` inside `policy dev`
fails at **check time**, not run time.

### M4 · Discovery and failure — `until dry(n)`, `retry/on`, `halt`
Loop primitive with mandatory budget guard (checker already has the
hook); `on BudgetExceeded ->` compensation blocks; `halt` as clean
typed termination.
**Accept:** a discovery loop terminates on n empty rounds AND on budget,
whichever first; partial results emitted on the budget path.

### M5 · The `@k` type system, statically
Move refinement from journal metadata into the checker: `[Fix@2]`
parameter types, subtype chain `@2 <: @1 <: @0`, call-site rejection.
This is the language's identity feature — worth its own audit pass.
**Accept:** passing unjudged findings to `fn apply(fixes: [Fix@2])` is a
check-time diagnostic with a fix-it hint naming the missing judge step.

### M6 · Testing story
`uta mission test`: scripted/recorded mock providers, golden-journal
assertions ("never raises git.write", "cost ≤ X", "gate fired once"),
replay of any recorded session as a regression test.
**Accept:** `examples/steer/` all run in CI against mocks with zero
network; one production trajectory converted into a passing test.

### M7 · Tooling
tree-sitter grammar (highlighting for the papers' code blocks too),
`uta mission fmt`, LSP-lite (diagnostics from the existing checker over
stdio). The time-travel debugger is a thin UI over the journal — schedule
after M6, it reuses the same replay machinery.

### M8+ · The frontier (from the 10-areas list, in dependency order)
1. Provenance lattice `@ {Security, Repro}` (extends M5's machinery)
2. Compensation/saga semantics (extends M4)
3. Streaming partial results (needs provider-stream plumbing — engine
   already consumes stream-json)
4. Cost inference from `perf` history (data exists; needs a model)
5. Memory decay / stale refinement (binds to `memory`/`recall`)
6. Richer human gates (choose/escalate/deadline — extends M3)
7. Session-typed agent protocols (research-grade; after `serve --mcp`
   missions-calling-missions works)
8. Featherweight steer — mechanized core calculus (Paper № 03 material)

## 2 · Build-with-uta protocol (how the work itself runs)

The repo is already a uta project. Division of labor that fits the tools:

**uta is the right tool when the work fans out or needs a verdict:**
- One construct per subtask once AST/walker interfaces are stable —
  M2/M3/M4 constructs are near-independent behind the `missionWalk`
  interface.
- Design decisions → `uta discuss` (Q1 lens-sets, Q3 dead-refuter
  semantics, M3 suspension representation), verdict recorded in ctx.
- Every milestone close → `uta audit internal/steer --mode audit`.
- Backlog grinding (docs, examples, error-message polish) →
  `uta improve` under a budget cap.

**A single deep session (Claude Code directly) is the right tool for:**
- Cross-cutting semantics: journal keying by structural position, replay
  identity, the M5 checker — one mind must hold the whole invariant.
- Anything touching `engine/mission.go`'s core walk loop.

Concrete setup (once):
```sh
cd ~/projects/uta
uta thread new steer-m2 && uta ctx put rfc docs/steer-language.md
uta ctx put plan docs/steer-implementation-plan.md
```

Per-milestone fan-out (example, M2+M4 constructs after interfaces are set):
```yaml
# steer-m2.yaml
version: 2
goal: implement judge and until-dry against the frozen walker interface
defaults: { worker: claude, max_parallel: 2 }
env: { ANTHROPIC_MODEL: claude-fable-5 }
subtasks:
  - id: judge
    title: judge stmt -> engine critics with vote counting
    prompt: |
      Implement JudgeStmt end to end: parser (replace the not-yet-implemented
      diag), walker case in internal/engine/mission.go mapping to the critic
      machinery with require-k-of-n counting, @k tag in the journal entry.
      Tests: quorum met, quorum missed, dead provider shrinks n.
      Interfaces in internal/steer/steer.go are FROZEN — extend, don't touch.
  - id: until-dry
    title: until dry(n) with mandatory budget guard
    prompt: |
      Implement UntilStmt: checker rule (refuse without budget guard in
      scope), walker loop with empty-round counting, termination on budget
      as the typed event. Tests for both exits.
```
Run: `uta run -f steer-m2.yaml --mode dev` · gate merges on
`uta audit internal/steer --mode audit` coming back clean.

Commit convention: continue the `P<n>: <construct> — <one-liner>` style
already in the log (P3, P4…), Co-Authored-By: uta <noreply@unleashtheagents.ai>.

## 3 · Risks

- **Replay identity under refactors** — journal keys derived from
  structural position break when programs are edited between runs.
  Decide early (M3): key = hash of (construct path + args), and document
  the invalidation rule. This is the sharpest knife in the drawer.
- **Scope creep via the papers** — the RFC promises more than v0 should
  ship. The "not yet implemented" diagnostic is the pressure valve; keep
  it honest.
- **Two type systems drifting** (runtime @k tags vs M5 static checks) —
  make the runtime tag the single source of truth; the checker only
  predicts it.
- **Engine coupling** — every construct lands in `mission.go`'s walk.
  Freeze a small walker interface after M1 so fan-out stays safe.
