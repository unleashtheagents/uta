# uta — unleash the agents

A CLI agent orchestrator. Decomposes goals into parallel subtasks, dispatches them to whichever coding-agent CLI you have installed (Claude Code, Gemini CLI, drop-in YAML descriptors for anything else), synthesizes the results — and records every step as a queryable trajectory.

Status: **pre-alpha**. v1 in active development.

## Why uta vs. just using Claude Code (or Gemini, or…)?

A single agent CLI is great at one back-and-forth conversation. uta is the layer above:

- **Parallelism.** A planner decomposes a goal into independent subtasks and fans them out. Three subtasks run on three providers concurrently instead of waiting in sequence.
- **Cross-provider.** Each subtask can target a different CLI — Gemini for repo-wide context (1M tokens), Claude for delegation-heavy edits, anything else via a 30-line YAML descriptor.
- **Replayable.** Every prompt, tool call, token count, and final answer is recorded in SQLite. `uta sessions` / `uta trajectory <id>` lets you inspect, audit, or resume any run later.
- **Modes & profiles.** A MissionProfile sets policy per task type (dev / ops / audit / research): allowed tools, HITL triggers, budget caps, retrospective cadence.
- **Bidirectional MCP.** uta can call MCP servers as tools, **and** expose itself as an MCP server so Claude Code, Cursor, etc. can call into uta's planner.

## Install

```sh
# Homebrew (macOS / Linux)
brew install unleashtheagents/tap/uta

# curl | sh
curl -fsSL https://unleashtheagents.ai/install.sh | sh

# Go toolchain
go install github.com/unleashtheagents/uta/cmd/uta@latest
```

## 60-second tour

```sh
uta hello                                             # what is this, what's installed, what to try next
uta doctor                                            # verify environment
uta providers                                         # list detected agent CLIs
uta run -g "summarize this repo in 5 bullets" -y      # ad-hoc orchestration
uta sessions                                          # list past runs
uta trajectory <id>                                   # the full event timeline
uta resume <id> -g "now turn each bullet into a tweet"
uta discuss -t "monolith or microservices for this repo?"   # two LLMs debate, moderator synthesizes
uta shell                                             # interactive shell: converse with + control agents
uta mission run examples/hello.steer                  # run a steer program (the agent language, v0)
```

`uta discuss` pits two agents — ideally two different LLMs (claude vs
gemini) — against each other for several rounds of rebuttals, then a
moderator synthesizes agreements, disagreements, and a verdict. See
[`docs/discussion.md`](docs/discussion.md).

`uta shell` is a persistent, terminal-style prompt on top of the same
engine: type a goal and it fans out; type again and the conversation
continues. Its slash commands are a superset of what the underlying agent
CLIs offer — `/model`, `/tools`, `/clear` work like in claude/gemini, plus
uta-level control like `/worker`, `/mode`, `/agents`, and `@gemini <goal>`
to route one turn to a specific agent. See [`docs/shell.md`](docs/shell.md).

`uta mission` is the v0 interpreter for **steer**, the programming language
for agents ([Paper № 02](https://unleashtheagents.ai/uta/steer/)): agent
calls are effects, budgets are linear and mandatory, and every run journals
to the normal session store. Hello world is at
[`examples/hello.steer`](examples/hello.steer); the guide is
[`docs/steer.md`](docs/steer.md).

Sample output of a fan-out run:

```text
$ uta run -g "summarize this repo in 5 bullets" -y
[uta] planner=claude/gemini worker=claude max-parallel=3
[uta] plan: 3 subtasks
  s1  what does this codebase do?
  s2  how is it structured?
  s3  what's the install path?
[uta] s1 ─ running (claude)         ✓ 1.2s
[uta] s2 ─ running (claude)         ✓ 2.1s   parallel with s1
[uta] s3 ─ running (claude)         ✓ 0.9s   parallel with s1/s2
[uta] synthesize ── claude          ✓ 0.7s
- A CLI for orchestrating multiple agent CLIs in parallel.
- ...

[uta] session 4f6c... status=completed  tokens=12,430  approx $0.04
```

Run `uta dash` for the at-a-glance view across all sessions, modes, and cost.

## Workflow file

```sh
uta init --workflow      # writes an example uta.yaml
uta run -f uta.yaml -y   # run it
```

`uta.yaml` lets you fix the worker/planner/synthesizer, pin subtasks per worker (e.g. Gemini for whole-repo context, Claude for delegation), or skip the planner entirely by listing subtasks inline.

## Add a new agent provider

Drop a YAML descriptor in `~/.uta/providers/`:

```yaml
name: codex
binary: codex
detect:
  args: ["--version"]
  version_regex: '(\d+\.\d+\.\d+)'
invocation:
  argv: ["exec", "--output-format", "{{output_format}}", "{{prompt}}"]
  resume_argv: ["exec", "--resume", "{{session_id}}", "{{prompt}}"]
output:
  format: stream-json
  session_id_field: session_id
  final_text_field: result
  event_dispatch:
    assistant: assistant_text
    tool_use:  tool_call
    tool_result: tool_result
capabilities: [resume, stream-json]
```

`uta providers` will pick it up at the next invocation.

To pin a provider to a specific model (e.g. run claude on Claude Fable 5,
or keep a cheap `gemini-flash` worker next to the default), see
[`docs/model-selection.md`](docs/model-selection.md) — covers per-workflow
env, per-mode env, and model-pinned wrapper providers.

## Use uta from another agent (MCP)

`uta serve --mcp` exposes the whole CLI as a Model Context Protocol server
on stdio. Wire it into Claude Code, Cursor, or any MCP-aware client and
their model can call into uta's planner, idea backlog, audit reflector,
and inter-agent whiteboard.

```sh
# Print a ready-to-paste client config:
uta serve --print-mcp-config claude-code > .mcp.json
uta serve --print-mcp-config cursor      > ~/.cursor/mcp.json
```

Full setup guide: [`docs/mcp-server.md`](docs/mcp-server.md). Sample
configs: [`examples/mcp/`](examples/mcp/).

## Layout

```
cmd/uta/                  # thin entrypoint
internal/cli/             # cobra commands
internal/engine/          # supervisor — orchestration core
internal/provider/        # AgentProvider interface + registry
  builtin/                #   claude.go, gemini.go
  declarative/            #   YAML-driven provider
internal/trajectory/      # fan-out event bus + SQLite recorder
internal/store/           # pure-Go SQLite + content-addressed blobs
internal/config/          # YAML schemas (workflow + provider descriptor)
internal/paths/           # ~/.uta resolution
```

## License

MIT — see `LICENSE`.
