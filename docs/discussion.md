# uta discuss — two agents debate a topic

`uta discuss` runs a structured, multi-round debate between two agents and
then has a moderator synthesize the outcome. It shines when the two sides run
on **two different LLMs** — different models have different priors, so the
disagreement is real, not role-played.

```sh
uta discuss -t "Should we migrate our monolith to microservices?"
```

With no `--agents`, uta pairs the first two distinct available providers
(e.g. `claude` vs `gemini`).

## How a debate runs

1. **Round 1** — Agent 1 opens with its position; Agent 2 reads the opening
   and responds.
2. **Middle rounds** — each turn sees the full transcript and must rebut the
   counterpart's strongest points, concede what's right, and add new
   arguments (repetition is explicitly discouraged).
3. **Final round** — both agents give closing statements: where they agree,
   where they still disagree and why, final position.
4. **Synthesis** — a moderator (default: the first agent's provider; override
   with `--synth`, disable with `--no-synth`) produces points of agreement,
   points of disagreement with a strength assessment, open questions, and its
   own verdict. The synthesis becomes the session's final answer.

Turns are threaded statelessly — every prompt embeds the transcript so far —
so any provider works, including declarative ones without reliable
session-resume.

## Flags

```
-t, --topic          the question under debate (required; positional args work too)
    --agents         exactly two provider names (default: first two distinct available)
    --rounds         full rounds, one turn per agent each (default 3, max 10)
    --personas       role per agent: a persona id from ~/.uta/personas/ or free text
    --synth          moderator provider (default: first agent)
    --no-synth       skip the moderator; the transcript is the output
    --turn-timeout   per-turn timeout (default 5m)
    --timeout        overall debate timeout (default 30m)
-o, --out            also write the markdown transcript to a file
    --json           structured result on stdout instead of the live transcript
```

The global `--mode` flag applies: the profile's `env:` is injected into every
turn (that's how you pin models — see below) and its budget policies
(`token_budget`, `dollar_budget_cents`, `per_call_budget`) cap the debate's
spend. Budget warnings surface inline; exhaustion ends the debate with
status `budget_exhausted`.

## Recipes

**Two different frontier models** (see [`model-selection.md`](model-selection.md)):

```sh
ANTHROPIC_MODEL=claude-fable-5 GEMINI_MODEL=gemini-2.5-pro \
  uta discuss -t "Is event sourcing worth it for our order system?" \
  --agents claude,gemini --rounds 4
```

**Adversarial roles on one provider** (personas make the sides distinct):

```sh
uta discuss -t "Adopt Kubernetes now?" --agents claude,claude \
  --personas "you argue FOR with urgency","you argue AGAINST from ops-burden experience"
```

**Persona files** — a value matching a persona id under `~/.uta/personas/`
expands to that persona's full prompt:

```sh
uta discuss -t "..." --personas security-auditor,performance-engineer
```

**Feed the verdict into a run:**

```sh
uta discuss -t "Which caching strategy?" --json > debate.json
uta run -g "Implement the moderator's verdict in debate.json" --worker claude -y
```

## Inspecting a debate afterwards

Every debate is a normal session — each turn is a subtask row:

```sh
uta sessions                      # the debate shows worker=claude+gemini
uta trajectory latest             # full event timeline incl. per-turn text
uta perf latest                   # latency + token/cost breakdown per turn
uta exportdb <id> -o debate.dump  # portable archive
```

Exit codes match `uta run`: `0` success, `4` a turn failed, `130` cancelled
(Ctrl-C; completed turns are already persisted).
