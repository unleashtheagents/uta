# uta shell — the interactive agent control plane

`uta shell` (alias: `uta repl`) is a persistent, terminal-style prompt on
top of the orchestration engine. Where `uta run` is one shot, the shell is a
session: type a goal, watch it fan out, type again and the same conversation
continues — every turn after the first resumes the prior session, so context
carries.

## The whole interface, abstractly

Every line you type at the `uta>` prompt is one of **four things**:

```
text          a goal — uta plans it, fans it out to agents, synthesizes an answer
@name text    the same, but routed to one specific agent for this turn
/command      control — steer the shell and the agents (who, which model, which rules)
!command      local — run it in your terminal; no agents involved
```

That's the entire mental model. Plain text is *conversation*, `/` is
*control*, `!` is *escape to the local shell*, `@` is *addressing*. The
slash commands are deliberately a **superset** of what the underlying agent
CLIs (claude, gemini) offer in their own REPLs — `/clear`, `/model`,
`/tools` do what those users already expect — plus cross-agent controls only
an orchestrator can have: switching the worker mid-conversation, pinning a
model per agent, routing single turns.

The control commands split into three small groups:

| Group | Commands | What they steer |
|---|---|---|
| conversation | `/new` `/clear` `/use` `/resume` `/sessions` | *which* conversation you're in |
| agent control | `/agents` `/worker` `/model` `/tools` `/mode` `/status` | *who* works, on *what model*, under *which rules* |
| terminal | `/cd` `/help` `/exit` | the shell itself |

## Hello world

```text
$ uta shell
[uta] shell — text talks to agents, /commands control them, !commands run locally. /help for details, /exit to leave.
[uta] worker: claude   mode: (none)   agents detected: claude, gemini

uta> hello — one sentence: what can you see in this directory?
[uta] plan: 1 subtask
[uta] s1 ─ running (claude)   ✓ 3.1s
A Go CLI project called uta with cobra commands under internal/cli and an
orchestration engine under internal/engine.

[uta] session 4f6c1b2a status=completed

uta [4f6c1b2a]> and how is it tested?
...the conversation continues — the agent still has the context...

uta [4f6c1b2a]> @gemini do you agree with claude's summary?
...one turn routed to gemini, same conversation...

uta [4f6c1b2a]> /exit
```

Three things happened there: the first line started a real session (the
prompt picked up its id), the second *resumed* it, and `@gemini` pulled a
second agent into the same conversation.

## How to use it, in five moves

1. **Converse.** Type goals. Each turn is a full orchestration (planner →
   fan-out → synthesis) recorded as a session — it shows up in
   `uta sessions`, `uta trajectory`, `uta perf`, and updates the active
   thread, so `uta resume` outside the shell picks up where you left off.
2. **Steer who works.** `/agents` lists detected providers, `/worker gemini`
   switches for good, `@gemini <goal>` for just one turn.
3. **Steer how they work.** `/model claude-fable-5` pins the model
   (per worker, via the provider's env var — see
   [model-selection.md](model-selection.md); `VAR=value` for custom
   wrappers). `/tools Read,Bash` pre-approves tools for every turn.
   `/mode deep-research` applies a MissionProfile — env, capability gates,
   HITL triggers, budgets.
4. **Manage conversations.** `/new` starts fresh, `/sessions` lists history,
   `/use 1a2b3c4d` re-enters any old conversation (short ids work).
5. **Stay in the terminal.** `!git diff` runs locally in the workdir;
   `/cd` shows or moves the directory agents operate in.

## Starting the shell

```sh
uta shell                              # first detected provider becomes the worker
uta shell --worker claude              # pin the worker
uta shell --session 1a2b3c4d           # re-enter an old conversation
uta shell --pre-approve Read,Bash      # tools pre-approved for every turn
uta shell --mode deep-research         # start under a MissionProfile
```

Per-turn limits mirror `uta run`: `--max-parallel`, `--max-subtasks`,
`--subtask-timeout`, `--timeout`.

## Command reference

| Input | Effect |
|---|---|
| `<text>` | Send a goal to the active agent. First input starts a run; later inputs resume the session. |
| `@<agent> <text>` | Route one turn to a specific agent without switching the default worker. |
| `/new`, `/clear` | Start a fresh conversation (next input starts a new run). |
| `/use <id>`, `/resume <id>` | Attach to an existing session; short ids from `/sessions` work. |
| `/sessions [n]` | List recent sessions (default 10). |
| `/agents` (alias `/providers`) | List agent providers, availability, and model pins; `*` marks the active worker. |
| `/worker [name]` | Show or switch the active worker agent. |
| `/model [m\|default]` | Show or pin the model the active worker runs on. Bare names map to the provider's env var (`ANTHROPIC_MODEL` for claude, `GEMINI_MODEL` for gemini); other providers need the explicit `VAR=value` form. Pins are per worker and win over a mode's `env:`. |
| `/tools [t,...\|none]` | Show or set the tools pre-approved for every turn. |
| `/mode [name\|none]` | Show, switch, or clear the active MissionProfile. The next turn picks up the new mode's env, gates, and budgets. |
| `/status` | Worker, session, model, tools, mode, workdir, and project at a glance. |
| `!<command>` | Run a local shell command in the workdir. |
| `/cd [dir]` | Show or change the workdir agents operate in. |
| `/help` | Command reference. |
| `/exit` | Leave the shell. `quit`, `/quit`, and Ctrl-D work too. |

## Behavior notes

- **Ctrl-C cancels the turn, not the shell.** The session stays attached, so
  the next input resumes the partially-run session; `/new` abandons it.
- A failed turn prints the error and returns to the prompt — it never tears
  down the shell.
- Inside a project (`.uta/project.yaml`), the workdir defaults to the
  project root and subtasks see the usual project env
  (`$UTA_CONTEXT_DIR`, `$UTA_ORG_STATE`, ...), exactly as with `uta run`.
- Switching `/worker` (or using `@agent`) mid-conversation resumes the same
  uta session with a different provider; the new agent gets the conversation
  context uta carries, not the old provider's internal state.
