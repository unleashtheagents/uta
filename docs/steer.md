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

Three commands, no tokens wasted on mistakes:

```sh
uta mission check hello.steer      # parse + static checks only
uta mission run hello.steer --dry-run   # print the plan, spend nothing
uta mission run hello.steer        # run it
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
- **Everything journals.** The mission is a session, every call is a
  subtask with prompt + raw output blobs and usage meta, every emit is an
  event — `uta sessions`, `uta trajectory`, `uta perf`, `uta exportdb`
  work unchanged. Program text + trajectory is the audit trail.
- **RFC constructs degrade legibly.** `par`, `judge`, `until dry`,
  `gate human`, `with cap` parse to a "not implemented in the v0
  interpreter" pointer, not a generic syntax error.

## The v0 subset

```
program    := agentFn* mission
agentFn    := "agent" "fn" name "(" (param ":" Type),* ")" "->" Type
              [ "worker" (name | "any(" name,* ")") ]
              [ "costs" "<=" N ["k"] "tokens" ]
              "prompt" """…${param}…"""
mission    := "mission" name "{" budget stmt* "}"
budget     := "budget" (N["k"] "tokens" | "$" N[".NN"] | N ("min"|"s"|"h")),*
stmt       := "let" name "=" expr | "emit" expr
expr       := "string" | name | call(expr,*)
```

Values are text in v0; declared types are contracts-in-waiting (arity is
checked, shapes come with schema validation in a later step). Calls may
nest: `emit shout(translate("hello"))` runs inner-first. Comments are
`//`; a leading `#!` shebang line is skipped.

## Where this is going

The paper's path to v0.x, in order: tree-sitter grammar, JSON Schemas from
type declarations, `@k` verification refinements, `par` fan-out, journaled
resume (a crash, a budget stop, and a human pause become the same
re-enterable state). The YAML workflow loader eventually becomes a
compatibility layer over `.steer`.
