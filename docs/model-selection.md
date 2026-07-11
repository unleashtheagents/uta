# Selecting the model a provider uses

uta orchestrates provider CLIs (`claude`, `gemini`, ...); it does not talk to
model APIs itself. Which *model* a worker runs on is therefore the provider
CLI's decision — and every supported CLI lets you steer that decision through
an environment variable or a flag. uta gives you three places to inject
either one, from most ad-hoc to most permanent.

## TL;DR

```sh
# One run of claude on Claude Fable 5 (env var, no config needed):
ANTHROPIC_MODEL=claude-fable-5 uta run -g "audit the auth flow" --worker claude -y
```

For anything you'll do twice, use one of the three mechanisms below.

## How each provider CLI picks its model

| Provider | Env var | Flag | Example values |
|---|---|---|---|
| `claude` (Claude Code) | `ANTHROPIC_MODEL` | `--model` | `claude-fable-5`, `claude-opus-4-8`, `claude-sonnet-4-6`, `claude-haiku-4-5` — the CLI also accepts short aliases like `fable`, `opus`, `sonnet`, `haiku` |
| `gemini` (Gemini CLI) | `GEMINI_MODEL` | `-m` / `--model` | `gemini-2.5-pro`, `gemini-2.5-flash` |
| declarative (`~/.uta/providers/*.yaml`) | whatever the wrapped CLI honors | bake into `invocation.argv` | provider-specific |

Model ids drift over time — check your provider CLI's own docs for the
current list. For Claude, prefer the dated-suffix-free aliases above
(`claude-opus-4-8`, not a date-suffixed variant).

## Option 1 — per-workflow: `env:` in `uta.yaml`

Workflow-declared env is passed to every subtask of that run. Good when a
particular workflow needs a stronger (or cheaper) model than your default.

```yaml
# uta.yaml
goal: "deep security audit of the contracts/ directory"
defaults:
  worker: claude
env:
  ANTHROPIC_MODEL: claude-fable-5
```

```sh
uta run -f uta.yaml
```

## Option 2 — per-mode: `env:` in a MissionProfile

Modes (`uta --mode <name>`, see `uta mode list`) carry an `env:` map that is
merged into every run made under that mode — workers, planners, and
synthesizers alike. This is the right home for "research runs use Fable,
routine runs use the default":

```yaml
# ~/.uta/profiles/deep-research.yaml  (or <project>/.uta/profiles/)
name: deep-research
description: long-horizon research runs on the most capable model
env:
  ANTHROPIC_MODEL: claude-fable-5
  GEMINI_MODEL: gemini-2.5-pro
```

```sh
uta run --mode deep-research -g "map the failure modes of our retry stack"
```

## Option 3 — permanent: a declarative provider wrapper

If you want the model to be a *worker identity* you can pick with
`--worker` (and mix in one run with other workers — planner on one model,
subtasks on another), wrap the CLI in a provider descriptor. Drop this into
`~/.uta/providers/claude-fable.yaml`:

```yaml
name: claude-fable
binary: claude
notes: Claude Code pinned to Claude Fable 5
invocation:
  # Mirrors the built-in claude invocation, plus the model pin.
  argv: ["--model", "claude-fable-5", "-p", "--output-format", "stream-json", "--verbose"]
  stdin: "{{prompt}}"
  resume_argv: ["--model", "claude-fable-5", "-p", "--resume", "{{session_id}}", "--output-format", "stream-json", "--verbose"]
  mcp_config_argv: ["--mcp-config", "{{mcp_config}}"]
  # env is an alternative to argv pinning; either works for claude:
  # env:
  #   ANTHROPIC_MODEL: claude-fable-5
output:
  format: stream-json
  session_id_field: session_id
  final_text_field: result
capabilities: [resume, stream-json, mcp]
```

Then:

```sh
uta providers                       # claude-fable shows up alongside claude
uta run -g "..." --worker claude-fable -y

# planner on the cheap model, workers on the strong one:
uta run -f uta.yaml --planner claude --worker claude-fable
```

The same pattern pins gemini: `binary: gemini`,
`argv: ["-m", "gemini-2.5-flash", ...]`, name it `gemini-flash`.

## Precedence and verification

- A flag baked into `argv` beats the env var (the CLI parses flags last), so
  an Option-3 wrapper wins over Options 1–2 for that worker.
- Env merging order for a run: workflow `env:` first, then mode `env:`
  (profiles are applied after workflow parsing), then project-context vars.
  Later entries win on duplicate keys.
- To verify what a run actually used: `uta run ... --print-jsonl` streams the
  provider's raw events — the init/system event carries the model name for
  both claude and gemini. Post-hoc, `uta trajectory <session-id> --format jsonl`
  shows the same events, and `uta perf --cost` breaks spend down by provider.

## Notes

- `ANTHROPIC_MODEL` / `GEMINI_MODEL` affect *every* invocation of that CLI in
  the run — planner, subtasks, and synthesis — unless a wrapper provider
  overrides a specific role via `--planner` / `--synth` / per-subtask `worker:`.
- Stronger models cost more per token. `uta perf --cost --since 7d` shows the
  rollup by provider/day; budget caps in a MissionProfile
  (`policies.token_budget`, `policies.dollar_budget_cents`) apply regardless
  of which model is behind the provider.
