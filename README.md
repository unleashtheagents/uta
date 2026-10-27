# uta — unleash the agents

A CLI agent orchestrator. Decomposes goals into parallel subtasks, dispatches them to whichever coding-agent CLI you have installed (Claude Code, Gemini CLI, drop-in YAML descriptors for anything else), synthesizes the results — and records every step as a queryable trajectory.

Status: **pre-alpha**. v1 in active development.

## Why uta vs. just using Claude Code (or Gemini, or…)?

A single agent CLI is great at one back-and-forth conversation. uta is the layer above:

- **Parallelism.** A planner decomposes a goal into independent subtasks and fans them out. Three subtasks run on three providers concurrently instead of waiting in sequence.
- **Cross-provider.** Each subtask can target a different CLI or cloud inference endpoint — **Nebius Token Factory (`nebius`)** running **NVIDIA Nemotron** models (`nvidia/Nemotron-3-Ultra-550b-a55b`, `nvidia/nemotron-3-super-120b-a12b`, `nvidia/Nemotron-3_5-Lightning`), Google Antigravity CLI (`agy`) for repo-wide context and deep reasoning, Claude Code for delegation-heavy edits, or anything else via a 30-line YAML descriptor.
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
uta providers                                         # list detected agent CLIs (claude, agy, etc.)
uta run -g "summarize this repo in 5 bullets" -y      # ad-hoc orchestration
uta run -g "build pipeline" --strategy dag           # DAG orchestration with explicit subtask dependencies
uta pr -g "add rate limiting" --verify 'go test ./...' # git branch + auto-commit + gh PR creation
uta serve --web --port 8080                           # interactive local Web Dashboard HTTP server
uta sessions                                          # list past runs
uta trajectory <id>                                   # the full event timeline
uta resume <id> -g "now turn each bullet into a tweet"
uta discuss -t "monolith or microservices for this repo?"   # two LLMs debate (claude vs agy), moderator synthesizes
uta shell                                             # interactive shell: converse with + control agents
uta nebius status                                     # Nebius Token Factory × NVIDIA Nemotron coordinator status
uta nebius models                                     # list Nemotron model tiers (ultra, super, lightning, vl)
uta nebius route "design a zero-trust MCP gateway"    # 90/10 Nemotron tier routing & cost breakdown
uta skills list                                       # discover workspace rules (AGENTS.md, CLAUDE.md) & SKILL.md
uta run -g "audit @internal/provider/builtin/nebius.go" --worker nebius # run on Nebius Token Factory with @file expansion
uta mission run examples/nebius-nemotron.steer        # run a Nebius × NVIDIA Nemotron steer mission
uta mission test examples/nebius-nemotron.steer --script examples/nebius-nemotron.journal.yaml
uta mission fmt examples/hello.steer                  # canonical formatting (diff; --write rewrites)
```

### Nebius Token Factory × NVIDIA Nemotron Coordinator (`nebius` provider & `uta nebius`)

`uta` ships with a built-in `nebius` provider and `uta nebius` coordinator that connects directly to **Nebius Token Factory** (`https://api.tokenfactory.nebius.com/v1`) to orchestrate all five **NVIDIA Nemotron** model tiers alongside local CLI agents (`claude`, `agy`, `gemini`):

| Alias | Model ID on Nebius Token Factory | Architecture & Active Params | Context | Role in `uta` |
|---|---|---|---|---|
| `ultra` | `nvidia/Nemotron-3-Ultra-550b-a55b` | Hybrid Mamba-2 + Transformer MoE (550B / 55B active) | 1M | **Planner & Synthesizer (10% tier)** — DAG decomposition, architectural review, and multi-agent verdict synthesis |
| `super` | `nvidia/nemotron-3-super-120b-a12b` | Hybrid Mamba-2 + Transformer MoE (120B / 12B active) | 1M | **Parallel Worker Swarm (90% tier)** — High-throughput coding subtasks (148 tok/s), test generation, and refactoring |
| `nano` | `nvidia/NVIDIA-Nemotron-3-Nano-30B-A3B` | Hybrid Mamba-2 + Transformer MoE (30B / 3B active) | 1M | **Fast Subtask & Summarizer Tier** — Rapid context compaction (`/compact`), commit synthesis, and lightweight iterations (295 tok/s) |
| `lightning` | `nvidia/Nemotron-3_5-Lightning` | Latent Mamba-Attention (30B / 3.5B active) | 256K | **Sentinel Guardrail & Fast Router** — Sub-50ms OWASP ASI01–ASI10 policy verification and prompt-injection screening (420 tok/s) |
| `vl` | `nvidia/Nemotron-3.5-Super-VL` | Multimodal Vision-Language MoE (120B / 12B + ViT) | 512K | **Multimodal UI & Diagram Analyst** — Visual architecture diagram auditing and UI screenshot inspection |

#### How Nebius Token Factory + Tavily Accelerate `uta`

1. **90/10 Hybrid Multi-Agent Routing (`uta nebius route`)**: `uta` routes 10% of tokens (DAG decomposition and final synthesis) to **Nemotron 3 Ultra** (`nvidia/Nemotron-3-Ultra-550b-a55b`) while fanning out 90% of parallel worker subtasks to **Nemotron 3 Super** (`nvidia/nemotron-3-super-120b-a12b`) and **Nemotron 3 Nano** (`nvidia/NVIDIA-Nemotron-3-Nano-30B-A3B`) on Nebius Token Factory — achieving **2.016s completion latency (5.5× faster than local CLI cold starts) and 134.4 tok/s live throughput** (`uta bench`).
2. **Autonomous Agentic Tool-Calling Loop (`UTA_NEBIUS_AGENTIC=1` / `-y`)**: When pre-approved (`-y` / `--preapprove` or `UTA_NEBIUS_AGENTIC=1`), the `nebius` provider exposes OpenAI-compatible function-calling tools (`read_file`, `write_file`, `edit_file`, `list_dir`, `run_command`, `tavily_search`) directly to NVIDIA Nemotron models on Nebius Token Factory so Nemotron agents can autonomously inspect files, edit code, run `go test ./...`, and iterate on a real repository with every tool call recorded in SQLite (`EventToolCall` / `EventToolResult`).
3. **Runtime Web Grounding via Tavily API (`uta nebius search` & `tavily_search`)**: `uta` integrates a zero-dependency runtime client (`internal/tavily`) for `POST https://api.tavily.com/search` so NVIDIA Nemotron agents can ground architectural plans and code changes in live documentation with citations (`uta nebius search "<query>"`) and persist findings to `.uta/context/RESEARCH_DATABASE.md`.
4. **Unified `agy` + `claude` Rules, Progressive Skills & `@file` Expansion**: Inspired by **Google Antigravity (`agy`)** and **Anthropic Claude Code (`claude`)**, `uta` discovers hierarchical project rules (`AGENTS.md`, `CLAUDE.md`, `GEMINI.md`, `.uta/rules/*.md`, `.agents/rules/*.md`), progressive skills (`skills/<name>/SKILL.md`), and expands `@path/to/file` mentions inside prompts before sending turns to Nebius Token Factory.

```sh
export NEBIUS_API_KEY="v1.nebius..."
export TAVILY_API_KEY="tvly-..."                      # optional: enables live Tavily search grounding
export NEBIUS_MODEL="ultra"                           # short alias: ultra | super | nano | lightning | vl
export UTA_NEBIUS_INJECT_RULES=1                      # auto-inject AGENTS.md / CLAUDE.md + progressive skills
uta nebius status                                     # inspect endpoint, auth, 90/10 tiers, and skills
uta nebius ping                                       # verify live connectivity against /v1/models
uta nebius models                                     # list all 5 NVIDIA Nemotron tiers on Nebius Token Factory
uta nebius route "design a 90/10 multi-agent router with Nemotron"
uta nebius search "NVIDIA Nemotron 3 Mamba-2 MoE benchmarks"
uta skills list                                       # inspect discovered AGENTS.md, CLAUDE.md, and SKILL.md files
uta run -g "audit @internal/provider/builtin/nebius.go and run tests" --worker nebius -y
uta mission run examples/nebius-nemotron.steer
```

> **Nebius × NVIDIA Global AI Hackathon (Aug 26 – Oct 30, 2026) Updates:** Built native `nebius` Token Factory provider (`internal/provider/builtin/nebius.go`, `nebius_tools.go`) supporting all 5 NVIDIA Nemotron tiers (`ultra`, `super`, `nano`, `lightning`, `vl`), SSE token streaming, multi-turn agentic tool calling (`read_file`, `write_file`, `edit_file`, `list_dir`, `run_command`, `tavily_search`), `uta nebius` coordinator CLI (`status`, `models`, `route`, `search`, `ping`), unified `agy`/`claude` progressive skills & rules discovery (`internal/skills`, `uta skills`), runtime Tavily Search API integration (`internal/tavily`), interactive shell `/plan`, `/compact`, `/nebius`, `/skills` slash commands, Web Dashboard `/api/nebius` & `/api/skills` cards, and `examples/nebius-nemotron.steer`. See [`docs/DEVPOST-NEBIUS-HACKATHON.md`](docs/DEVPOST-NEBIUS-HACKATHON.md) for the complete Devpost guide.

`uta pr` runs an autonomous dev mission on a dedicated Git branch (`uta/<slug>`), validates it against verification gates, commits changes, and opens a Pull Request via `gh pr create` with a structured trajectory summary.

`uta serve --web` launches an interactive local Web Dashboard HTTP server (`http://127.0.0.1:8080`) providing real-time stats, session tracking, and provider status.

`uta discuss` pits two agents — ideally two different LLMs (claude vs
agy) — against each other for several rounds of rebuttals, then a
moderator synthesizes agreements, disagreements, and a verdict. See
[`docs/discussion.md`](docs/discussion.md).

`uta shell` is a persistent, terminal-style prompt on top of the same
engine: type a goal and it fans out; type again and the conversation
continues. Its slash commands are a superset of what the underlying agent
CLIs offer — `/model`, `/tools`, `/clear` work like in claude/agy, plus
uta-level control like `/worker`, `/mode`, `/agents`, and `@agy <goal>`
to route one turn to a specific agent. See [`docs/shell.md`](docs/shell.md).

`uta mission` is the v0 interpreter for **steer**, the programming language
for agents ([Paper № 02](https://unleashtheagents.ai/uta/steer/)): agent
calls are effects, budgets are linear and mandatory, and every run journals
to the normal session store. Alongside `run` there are `check` (parse +
static checks, no spend), `test` (scripted responses with golden-journal
assertions — no provider, no network), and `fmt` (one canonical shape).
Hello world is at [`examples/hello.steer`](examples/hello.steer); the
guide is [`docs/steer.md`](docs/steer.md).

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
internal/cli/             # cobra commands (run, pr, shell, mission, serve, dash, replay, bench, ...)
internal/engine/          # supervisor — orchestration core (fanout, dag, race, reflector, mission)
internal/steer/           # steer language lexer, parser, static checker, formatter, schema
internal/provider/        # AgentProvider interface + registry
  builtin/                #   claude.go, agy.go, gemini.go
  declarative/            #   YAML-driven provider
internal/trajectory/      # fan-out event bus + SQLite recorder
internal/store/           # pure-Go SQLite + content-addressed blobs
internal/config/          # YAML schemas (workflow + provider descriptor)
internal/paths/           # ~/.uta resolution
```

## License

MIT — see `LICENSE`.
