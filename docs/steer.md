# steer v0 — uta as the interpreter

steer is the programming language for agents sketched in
[Paper № 02](https://unleashtheagents.ai/uta/steer/) (RFC source:
[`steer-language.md`](steer-language.md)): a layer above the uta control
verbs where **inference is an effect, verification is a type, budget is a
linear resource, and a human is an awaitable gate**. uta ships the v0
interpreter the paper calls for: `uta mission run` parses a `.steer`
program, walks it, turns every agent fn call into a provider subtask, and
journals everything to the normal session/trajectory store.

> steer is to the uta verbs what C is to assembly: same machine, structured
> authorship.

## Hello, world

```steer
#!/usr/bin/env -S uta mission run
// hello.steer — the first steer program.

agent fn greet(name: Text) -> Text
  worker any(claude, gemini)     // a provider set, not a vendor lock
  costs <= 50k tokens            // per-call ceiling, enforced
  prompt """
  Say "hello, ${name}" back — one short, warm sentence.
  No preamble, no quotes.
  """

mission hello_world {
  budget 200k tokens, $1, 5min   // linear; a mission without a budget won't compile

  let greeting = greet("world")
  emit greeting                  // the mission's public result
}
```

```text
$ uta mission run hello.steer
→ mission hello_world
  session=e5e3ffc7
  ✓ Done in 6s, 21.2k tokens

Hello, world — it's lovely to hear from you!

[uta] mission hello_world session e5e3ffc7 status=completed calls=1 duration=6s tokens=21192 cost=$0.05
```

Five commands, no tokens wasted on mistakes:

```sh
uta mission check hello.steer      # parse + static checks only
uta mission run hello.steer --dry-run   # print the plan, spend nothing
uta mission run hello.steer        # run it
uta mission test hello.steer --script canned.yaml   # scripted responses — no provider, no tokens
uta mission fmt hello.steer        # canonical formatting (prints a diff; --write rewrites)
chmod +x hello.steer && ./hello.steer   # the shebang makes it a script
```

Also available: `--print-jsonl` streams every trajectory event to stdout
(parity with `uta run`); `--mode <name>` merges a MissionProfile's env —
model pins, credentials — into every call, while budget authority stays
with the program text ("cost is spoken in the sentence"); and inside
`uta shell`, `/mission [check] <file.steer>` runs a program with the
shell's worker and workdir.

The example lives at [`examples/hello.steer`](../examples/hello.steer).

## What the interpreter gives you (v0)

- **Compile-time checks before any spend.** Unknown functions (with
  did-you-mean suggestions), arity mismatches, `${slot}` names that aren't
  parameters, duplicate bindings — all reported compiler-style with
  `file:line:col`, a source excerpt, and a caret.
- **Budget is mandatory and linear.** A mission without a token or dollar
  ceiling refuses to compile. Every call debits the shared budget;
  exhaustion is a typed, journaled event (`status=budget_exhausted`,
  exit 5) — not an invoice.
- **Per-call ceilings.** `costs <= 50k tokens` on an agent fn is enforced
  per call. Note: agent CLIs count their whole context (system prompt,
  cache reads) as usage — a trivial claude call reports ~20k tokens, so
  size ceilings accordingly.
- **A duration budget is a deadline.** `budget …, 5min` bounds the whole
  mission wall-clock.
- **Provider sets, not vendor locks.** `worker any(claude, gemini)`
  resolves to the first available provider at run time; the error when none
  is available tells you what was asked for and what was detected.
- **Typed records, enforced at the boundary.** A `type` declaration
  compiles to a JSON Schema; when an agent fn returns that type, the
  expected shape is appended to the prompt and the reply is validated
  before it flows anywhere. A mismatch means a bounded retry, then a
  typed `SchemaMismatch` failure that `retry n on E` can catch. See
  [`examples/records.steer`](../examples/records.steer).
- **Everything journals.** The mission is a session, every call is a
  subtask with prompt + raw output blobs and usage meta, every emit is an
  event — `uta sessions`, `uta trajectory`, `uta perf`, `uta exportdb`
  work unchanged. Program text + trajectory is the audit trail.
- **The paper's control constructs execute.** `par for` fan-out,
  `judge … require k of n` verification, `gate human` suspension,
  `with cap … from policy` capability raises, `until dry(n)` discovery
  loops, `retry n on E`, and mission-level `on E ->` handlers are all
  interpreted in v0 — each detailed below.
- **RFC constructs beyond the subset degrade legibly.** Host `fn`,
  `context`, `recall`, `var`, and `if` parse to a "not implemented in
  the v0 interpreter" pointer, not a generic syntax error.

## The v0 subset

```
program    := (typeDecl | agentFn)* mission
typeDecl   := "type" Name "{" (field ":" ("Text"|"Int"|"Bool")),* "}"
agentFn    := "agent" "fn" name "(" (param ":" Type),* ")" "->" (Type | "[" Type "]")
              [ "worker" (name | "any(" name,* ")") ]
              [ "costs" "<=" N ["k"] "tokens" ]
              "prompt" """…${param}…"""
mission    := "mission" name "{" ["policy" name] budget stmt* onHandler* "}"
budget     := "budget" (N["k"] "tokens" | "$" N[".NN"] | N ("min"|"s"|"h")),*
stmt       := "let" name "=" expr ["retry" N "on" Err]
            | "emit" expr
            | "gate" "human" expr
            | "with" "cap" "string" "from" "policy" name "{" stmt* "}"
            | "until" "dry" "(" N ")" "{" stmt* "}"
onHandler  := "on" Err "->" stmt (";" stmt)*      // body may also "halt"
expr       := "string" | name | call(expr,*)
            | "par" "for" name "in" "[" expr,* "]" "{" expr "}"
            | "judge" expr "by" call,* "require" K "of" N
```

`judge` is k-of-n verification: the verifiers run concurrently, each
receiving the judged value as its **last declared parameter** (so a
2-param verifier is written with 1 argument in the `by` list) plus the
STANDS/REFUTED verdict contract ("default to REFUTED if uncertain"). The
value passes through when at least K verifiers answer STANDS; unclear
responses and dead verifiers count against it; fewer than K is a typed
`judge rejected` failure with the full tally journaled.

`par for` is structured fan-out: the body runs once per item,
concurrently (bounded by `--max-parallel`, default 4), and the
expression's value is the branch results joined in item order. Each
branch journals under a structural spec id (`p1.b2.c1-scan`), so
`--resume-session` recovers completed branches deterministically no
matter how the scheduler interleaved them. List literals are only legal
as the iteration source — the v0 value model stays Text. See
[`examples/fanout.steer`](../examples/fanout.steer).

`gate human` suspends the mission until a human signs off: uta writes a
pending HITL request, closes the session as awaiting-signature, and
exits cleanly — the state survives a process restart. `uta hitl approve
<session>` (or `--deny`) records the decision, then `uta mission run
<file> --resume-session <session>` carries the walk past the gate;
already-paid calls are recovered from the journal, not re-paid. A denial
is a typed `GateDenied` failure. See
[`examples/gated.steer`](../examples/gated.steer).

`with cap C from policy P` raises one capability for exactly one block:
inside it the named tool pattern joins the allow set of the mission's
capability gate, and on exit the raise is dropped. The checker verifies
at `uta mission check` time that policy `P` actually grants `C` — an
unbacked raise never reaches run time — and the runtime journals a
cap_raised / cap_dropped event pair so the trajectory shows exactly
which calls ran with elevated authority.

`until dry(n)` is the discovery loop: the body runs in rounds until n
consecutive rounds come up dry (nothing new yielded) or the mission
budget runs out, whichever comes first. Budget exhaustion terminates the
loop, not the mission — results bound so far stay visible, so a
following emit publishes the partial yield. The checker refuses the
construct in a mission without a token or dollar ceiling.

Failure is typed. `let x = fix(plan) retry 2 on SchemaMismatch`
re-evaluates a failed expression only for the named error, bounded;
mission-level `on BudgetExceeded -> emit partial; halt` handlers run
compensation statements with already-bound lets still visible, and
`halt` turns the failure into a clean typed termination.

Declared record types are enforced, not decorative: the expected JSON
shape is appended to the agent's prompt and the reply is validated at
the boundary (mismatch → bounded retry → typed `SchemaMismatch`).
Between the seams, values flow as text — a `[Finding]` moves onward as
its JSON. Calls may nest: `emit shout(translate("hello"))` runs
inner-first. Comments are `//`; a leading `#!` shebang line is skipped.

## Testing and formatting

`uta mission test <file> --script <yaml>` interprets the program exactly
like `run`, but every agent fn call is answered from a YAML script of
canned responses — no provider, no network, no tokens — against a
throwaway journal, with `gate human` auto-approving. The script may pin
the expected final emit and assert against the journal:

```yaml
responses:
  greet:
    - "hello, world"   # served in call order; a bare string works for one-call fns
expect: "hello, world"
assert:
  calls:               # exact invocation count per agent fn
    greet: 1
  gates_fired: 1       # number of `gate human` statements executed
  max_cost_usd: 0.5    # cap on summed per-call spend from the journal
```

`--from-session <id>` derives the script from a recorded mission run
instead: each completed call's result is replayed for the agent fn whose
prompt template produced it — a production trajectory as a regression
test. (The shipped `examples/*.steer` are guarded the same way in CI:
parse/check in `internal/steer/examples_test.go`, executed end-to-end
against scripted providers in `internal/engine/examples_test.go`.)

`uta mission fmt <file>` formats a program into the one canonical shape
(two-space indent, worker/costs/prompt clause order, normalized budget
lists), printing a unified diff by default; `--write` rewrites the file
and preserves a `#!` shebang line.

## Where this is going

Schema-enforced types, `par` fan-out, judgment, gates, capability
raises, discovery loops, and journaled resume (a crash, a budget stop,
and a human pause are the same re-enterable state) have all landed — see
[`steer-implementation-plan.md`](steer-implementation-plan.md) for the
verified milestone state. Next, in order: the `@k` verification
refinements as a static type system (passing unjudged findings to a
`[Fix@2]` parameter becomes a check-time diagnostic), then tooling —
tree-sitter grammar, LSP-lite diagnostics over stdio. The YAML workflow
loader eventually becomes a compatibility layer over `.steer`.
