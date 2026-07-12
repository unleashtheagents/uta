# RFC · steer — a programming language for controlling agents

Status: **draft sketch** · 2026-07-12 · published as Paper № 02: https://unleashtheagents.ai/uta/steer/ · companion to Paper № 01
("The agent control command language", unleashtheagents.ai/uta/control-language/)

Paper № 01 argued that agent control needs a shared *command* grammar — verbs
you type. This RFC goes one level up: when the control logic itself gets
complex (conditionals on verdicts, discovery loops, compensation, budget
arithmetic), verbs glued with shell and YAML stop scaling, and you need a
*language*. Working name: **steer** (`*.steer`), after the site's own
imperative: unleash the agents, then steer them.

---

## Part I — The need

Eight arguments, ordered from practical to structural.

### 1 · The CLI ceiling
`uta run … && uta audit … || uta resume …` is shell programming. Shell
loses everything between the pipes: no types across steps, no shared error
model, no way to say "if two of three critics disagree, widen the search."
Every operator reinvents the same bash scaffolding around the verbs — the
historical signal that a language wants to exist (sh → perl → make → nix).

### 2 · The YAML ceiling
Workflow files express static shapes: fixed subtasks, fixed fan-out, one
strategy. The moment you need *dynamic* structure — loop until two dry
rounds, fan out one verifier per finding, escalate only the high-severity
half — you end up encoding a program inside YAML strings. A DAG file is a
photograph of a program; agent work needs the program.

### 3 · Prompts are not programs
Natural language carries intent brilliantly and contracts terribly. You
cannot type-check a prompt, diff its behavior, or test its edge cases. The
orchestration logic that today lives *inside* prompts ("first do X, if that
fails try Y, never touch Z") is exactly the part that should be lifted into
code — checkable, reviewable, versionable — leaving prompts to do the one
thing they are good at: describing goals.

### 4 · General-purpose languages mis-model the callee
Python calling an LLM pretends a stochastic, dollar-costing, minutes-slow,
fallible operation is an ordinary function call. Everything that matters —
retries, output validation, verification, cost accounting, resumption — 
becomes library boilerplate, and the library ecosystem (LangChain and its
descendants) is a decade of fighting the host language's assumption that
calls are cheap, fast, and deterministic. The fix is a language whose call
semantics match the callee: **inference is an effect**, not a function.

### 5 · Verification belongs in the type system
Models hallucinate; therefore the interesting property of a result is not
its shape but its *provenance*: who produced it, who tried to refute it, how
many refuters failed. A language for agents should let you write
`Finding@2` — a finding that survived two independent adversarial checks —
and let the compiler refuse to pass an unverified claim to an action that
demands a verified one. Today this discipline lives in engineers' heads.

### 6 · Budget is the memory-safety of the agent era
Tokens, dollars, and deadlines are the resources agents leak. "Surprise
bill" is the new segfault. A language can make the spend envelope part of
every call's signature, forbid unbounded loops that lack a budget guard,
and account linearly — so running out is a typed, catchable event, not an
invoice.

### 7 · Least authority needs a compiler
Prompt injection turns any agent into a confused deputy. The defense —
capability discipline, tools granted per-call, no ambient authority — is
exactly what object-capability languages solved. A mode/policy should not
be a runtime hope; it should be a *scope* the compiler checks: this
mission, under this policy, simply has no `git.push` capability to steal.

### 8 · The program should be the audit trail
Compliance for agent fleets means replayable evidence. If every construct
in the language journals by definition (Paper № 01, rule R1), then the
program text plus its trajectory *is* the compliance story — no
re-warehousing, no screenshot forensics. That property cannot be bolted
onto a general-purpose language; it must hold for every reachable
expression, which means the language must be closed over journaled effects.

**Counter-argument, honestly held:** "just use Python + a good SDK." The
answer is the same as for SQL, make, HCL, and regex: when the domain's
semantics (sets, dependencies, infrastructure, patterns — here:
nondeterminism, cost, verification, resumption) diverge far enough from
general-purpose call semantics, a small domain language beats a large
library, because the *compiler* can enforce what the library can only
document.

---

## Part II — The skeleton

### Design commitments (inherited from Paper № 01's nine rules)

1. Every effect journals; every mission is resumable from its journal.
2. Policy is a scope; capabilities are values; no ambient authority.
3. Budget is linear and explicit; unbounded iteration requires a guard.
4. Verification is a type refinement, not a comment.
5. Humans are an awaitable expression (`gate`), inside the language.
6. Providers are interchangeable arguments; swapping vendors never
   changes program structure.
7. Small enough to read back. Not general-purpose — deterministic host
   work stays in host functions.

### The three callables

```steer
// 1 · deterministic host function — no inference, no journal entry needed
fn dedupe(findings: [Finding]) -> [Finding]

// 2 · agent function — body is a goal, execution is inference (an effect)
agent fn find_bugs(dim: Lens, brief: Text) -> [Finding]
  worker any(claude, gemini)          // provider set, not a vendor lock
  costs <= 80k tokens                 // per-call ceiling, enforced
  prompt """
    Review the repository for ${dim} bugs.
    Context brief: ${brief}
  """

// 3 · judge function — an agent fn whose return refines its argument's type
judge fn refute(lens: Lens) verdict (f: Finding) -> Refuted | Stands
  prompt """
    Try to refute the finding ${f} through the ${lens} lens.
    Default to Refuted if uncertain.
  """
```

Agent outputs are schema-validated at the boundary (declared return type =
the contract; mismatch → bounded retry → typed failure). The prompt is the
*only* place natural language lives.

### A mission, end to end

```steer
mission review_release {
  policy dev                          // grants caps: fs.read, git.diff — not git.push
  budget 800k tokens, $12, 45min     // linear; overrun is a catchable event

  context brief = load("SPEC.md")    // named blob, journaled

  // dynamic fan-out — one branch per lens, structured concurrency
  let raw = par for dim in [Security, Perf, Correctness] {
    find_bugs(dim, brief)
  }.flatten() |> dedupe

  // verification as type refinement:
  // [Finding] -> [Finding@2] (survived 2 of 3 refuters)
  var real = judge raw
             by refute(Security), refute(Repro), refute(Correctness)
             require 2 of 3

  // discovery loop primitive — runs until 2 consecutive empty rounds,
  // illegal without a budget guard in scope
  until dry(2) {
    let more = find_bugs(Security, brief) |> dedupe_against(raw)
    real += judge more by refute(Repro), refute(Security) require 2 of 2
  }

  if real.filter(.severity == High).any() {
    let fixes = fix(real.high)
                retry 2 on SchemaMismatch
                on BudgetExceeded -> emit partial_report(real); halt

    gate human "apply ${fixes.len} fixes to main?"   // suspends; hitl resumes
    with cap git.write from policy release {          // capability, explicitly raised
      apply(fixes)
    }
  }

  emit report(real)                   // typed artifact into the context store
}
```

### Core constructs

| construct | semantics |
|---|---|
| `mission` | top-level unit; journaled, resumable, addressable (= a session) |
| `policy P` | static scope: personas, capability grants, HITL triggers (= a mode) |
| `budget T, $, D` | linear resources; every effect debits; exhaustion → typed event |
| `context k = …` | named blob binding, shared with all agent calls (= ctx) |
| `par { } / par for` | structured fan-out; combinators `all`, `first`, `quorum(k)` |
| `judge … by … require k of n` | refinement: output type gains `@k` verification level |
| `until dry(n)` | discovery loop; terminates on n empty rounds; requires budget guard |
| `gate human "…"` | suspension point; resumes via `hitl approve` with same journal |
| `retry n on E` / `on E ->` | typed failure handling; compensation blocks |
| `with cap C from policy P` | explicit, scoped authority raise; compiler-checked |
| `emit` | typed artifact out; the mission's public result |
| `recall "query"` | query institutional memory, returns ranked bindings |

### Type system sketch

- **Records** declared like `type Finding { title: Text, file: Path, severity: Sev }` — double as JSON Schemas at the agent boundary.
- **Verification refinements**: `Finding@k` (k independent refuters failed to kill it). Subtyping: `Finding@2 <: Finding@1 <: Finding`. Actions can demand levels: `fn apply(fixes: [Fix@2])`.
- **Resources**: `Tokens`, `Money`, `Duration` are linear — spent, not copied.
- **Capabilities**: `cap git.write`, `cap net.fetch(domain)` are unforgeable values held by policies; calls that need them don't compile in scopes that lack them.
- **Effects**: `!infer`, `!tool(C)`, `!human`, `!clock` — a `fn` has none, an `agent fn` has `!infer` at minimum. Purity is checkable.

### Grammar skeleton (EBNF, abridged)

```ebnf
program    = { decl } ;
decl       = mission | agentfn | judgefn | hostfn | typedecl | policydecl ;
mission    = "mission" IDENT block ;
agentfn    = "agent" "fn" sig [ "worker" workers ] [ "costs" bound ] "prompt" STRING ;
judgefn    = "judge" "fn" sig "verdict" sig "prompt" STRING ;
hostfn     = "fn" sig [ block ] ;
sig        = IDENT "(" [ params ] ")" [ "->" type ] ;
type       = IDENT [ "@" INT ] | "[" type "]" | type "|" type ;
stmt       = letstmt | parstmt | judgestmt | untilstmt | ifstmt
           | gatestmt | withcap | emitstmt | exprstmt ;
letstmt    = ( "let" | "var" ) IDENT "=" expr | IDENT "+=" expr ;
parstmt    = "par" [ "for" IDENT "in" expr ] block [ combinator ] ;
combinator = "." ( "all" | "first" | "quorum" "(" INT ")" | "flatten" "(" ")" ) ;
judgestmt  = "judge" expr "by" calllist "require" INT "of" INT ;
untilstmt  = "until" "dry" "(" INT ")" block ;
gatestmt   = "gate" "human" STRING ;
withcap    = "with" "cap" IDENT "from" "policy" IDENT block ;
handler    = "retry" INT "on" IDENT | "on" IDENT "->" ( block | stmt ) ;
```

### Execution model

Deterministic replay, Temporal-style but linguistic: the interpreter walks
the AST; every effect (`!infer`, `!tool`, `!human`) writes a journal entry
keyed by its structural position; re-running a mission with an existing
journal returns recorded results for completed effects and executes only
the frontier. `gate human` is just an effect whose result arrives later.
Paid work is never re-paid (rule R4) — as a semantics, not a feature.

### Mapping to uta today (the compile target)

| steer | uta v0.12 |
|---|---|
| `mission` run | `uta run` session |
| `agent fn` call | subtask on a provider |
| `judge … require k of n` | `audit` / `discuss` with vote counting |
| `gate human` | `hitl` request/approve |
| `context` / `emit` | `ctx put` / `ctx get` |
| `policy` | mode (MissionProfile) |
| `recall` | `recall` |
| journal | trajectory (JSONL/OTLP) |
| `budget` | mode budgets + `perf --cost` |

steer is to the uta verbs what C is to assembly: same machine, structured
authorship. v0 can be an interpreter inside uta (`uta mission run x.steer`)
that shells out to the existing verbs; native execution comes later.

### Deliberately not in the language

- General-purpose IO, arbitrary imports, FFI beyond declared `fn` hosts —
  ambient authority is the vulnerability class we exist to remove.
- Unbounded recursion / `while true` without a budget guard in scope.
- Prompt manipulation at runtime (no string-building of prompts outside
  `prompt` blocks — injection surface stays enumerable).
- A package manager, for as long as humanly possible.

### Open questions

1. Verification levels compose how? (`@2` from *which* lenses — should the
   type carry the lens set, not just the count?)
2. Are policies static enough for compile-time capability checking when
   modes can be edited per-project at runtime?
3. Quorum semantics under provider failure — does a dead refuter count
   against `require k of n`?
4. Syntax for streaming/partial results from long agent calls.
5. Whether `judge` should be able to *demote* (strip refinement from
   memory-recalled facts whose sources have aged).

### Path to v0

1. **Spec + grammar** (this doc → tree-sitter grammar, syntax highlighting).
2. **Interpreter** in Go inside uta: parse, walk, shell out to verbs,
   journal to the existing trajectory store. — **shipped**: `uta mission
   run|check` executes the hello-world slice (`internal/steer/` +
   `engine.RunMission`); see [steer.md](steer.md) and
   [`examples/hello.steer`](../examples/hello.steer).
3. **Static checks**: capability scopes, budget-guard-required loops,
   schema generation from `type` decls. — *partially shipped*: mandatory
   mission budgets, `${slot}`/arity/name resolution, did-you-mean
   diagnostics with source excerpts.
4. **Refinement enforcement** at boundaries (`@k` levels).
5. Dogfood: rewrite `examples/workflows/*.yaml` as `*.steer`; the YAML
   loader becomes a compatibility layer.
