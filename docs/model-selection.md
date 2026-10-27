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

## Running local models (Ollama)

uta drives *CLIs*, not model APIs, so a local model reaches uta the same way
any other does: through a CLI wrapped in a declarative provider. [Ollama](https://ollama.com)
is the runtime; how you wrap it depends on what you want the model to *do*.
Bake the model tag straight into `argv` — one descriptor per model (from
`ollama list`).

### Text / reasoning roles — wrap `ollama run`

Best for planners, synthesizers, `uta discuss`, or any text-in/text-out
subtask. `ollama run <model>` reads the prompt on stdin and prints plain
text. Drop this into `~/.uta/providers/llama.yaml`:

```yaml
name: llama
binary: ollama
notes: Ollama llama3.3 (local general model)
invocation:
  argv: ["run", "llama3.3:latest"]
  stdin: "{{prompt}}"
output:
  format: text
```

These providers have **no tools** — they can't edit files or run commands,
only produce text. And because declarative providers don't report usage,
**dollar budget caps never trip on them** (fine — local inference is free).
Prefer `llama3.3` over reasoning models like `deepseek-r1` for orchestration
roles: `deepseek-r1` emits `<think>…</think>` tokens that muddy a planner's
JSON output.

### File-editing role — wrap `aider` pointed at Ollama

For a local model that actually edits files, you need a real agent CLI with
a tool loop; raw `ollama run` can't. [aider](https://aider.chat) edits files
(git-aware), runs headlessly, and talks to Ollama's OpenAI-compatible
endpoint (`http://localhost:11434`). Install it (`brew install aider`), then
`~/.uta/providers/aider-llama.yaml`:

```yaml
name: aider-llama
binary: aider
notes: aider + Ollama llama3.3 (local, edits files)
invocation:
  argv:
    - "--model"
    - "ollama_chat/llama3.3:latest"   # llama3.3, not deepseek-r1: reasoning
                                       # models mangle aider's edit format
    - "--message"
    - "{{prompt}}"
    - "--yes-always"                   # auto-confirm file adds + edits
    - "--no-auto-commits"              # leave git history to you
    - "--no-stream"
    - "--no-check-update"
    - "--no-analytics"
  env:
    OLLAMA_API_BASE: "http://127.0.0.1:11434"
output:
  format: text
```

`aider-llama` is a **worker (editor), not a planner or synthesizer** — keep
those roles on a fast model or aider will be asked to "plan" and produce
junk:

```sh
# claude plans fast, your LOCAL model does the edits:
uta run -g "rename greet() to welcome() across the package" \
  --worker aider-llama --planner claude --synth claude --subtask-timeout 20m -y
```

Expect a 70B local model to be slower (the first call per model pays a
~cold-load of the weights into RAM — keep `--subtask-timeout` generous) and
to need more retries on large multi-file edits than claude/gemini. If aider
rejects edits on a run, add `--edit-format whole` to the `argv` — it trades
token cost for a format small local models rarely mangle.

### `--verify`: test-in-the-loop is the biggest quality lever

A weak model's edits get *trustworthy* when a ground-truth check runs after
them. `--verify "<cmd>"` executes a command in the workdir after the run and,
on non-zero exit, feeds the output back to the worker for a bounded repair
loop (`--verify-retries`, default 2):

```sh
uta run -g "fix the failing add() in calc.py" \
  --worker aider-llama --planner claude --synth claude \
  --verify "python3 -m pytest -q" --verify-retries 3 --subtask-timeout 20m -y
```

The compiler/test suite compensates for what the model guesses wrong — a
mediocre local editor with a check in the loop beats a strong one editing
blind. Since latency is a non-issue in a batch/overnight run, more repair
attempts is pure upside. `uta run` exits `6` if the check still fails after
the retries. (This is the one-flag form of the per-subtask `gate:` in a DAG
workflow.)

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

## Disabling a provider

Sometimes a provider's CLI is installed but you don't want uta to use it —
no credits, a paused subscription, a policy. Set `UTA_DISABLED_PROVIDERS`
(comma-separated, case-insensitive):

```sh
export UTA_DISABLED_PROVIDERS=gemini
```

A disabled provider is never registered: it disappears from
`uta providers`, is not offered in the worker picker, and is never
auto-selected by defaults like `uta discuss`'s first-two-available rule or
a workflow's `worker: any(...)` set. Unset the variable to re-enable —
nothing else changes.
