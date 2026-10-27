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
uta auth set nebius                                   # store an API key (prompted, no echo); uta auth status
uta limits set nebius --daily-usd 5                   # per-provider spend caps; uta limits status
uta nebius models                                     # list Nemotron model tiers (ultra, super, nano, lightning, vl)
uta nebius route "design a zero-trust MCP gateway"    # 90/10 Nemotron tier routing
uta nebius cost <id>                                  # tokens and USD per Nemotron tier/role vs all-Ultra
uta skills list                                       # discover workspace rules (AGENTS.md, CLAUDE.md) & SKILL.md
uta run -g "audit @internal/provider/builtin/nebius.go" --worker nebius # run on Nebius Token Factory with @file expansion
uta mission run examples/nebius-nemotron.steer        # run a Nebius × NVIDIA Nemotron steer mission
uta mission test examples/nebius-nemotron.steer --script examples/nebius-nemotron.journal.yaml
uta mission fmt examples/hello.steer                  # canonical formatting (diff; --write rewrites)
```

### Nebius Token Factory × NVIDIA Nemotron Coordinator (`nebius` provider & `uta nebius`)

`uta` ships a built-in `nebius` provider that talks directly to **Nebius Token Factory** (`https://api.tokenfactory.nebius.com/v1`) and treats the five **NVIDIA Nemotron** tiers as one coordinated team next to your local CLI agents (`claude`, `agy`, `gemini`):

| Alias | Model ID on Nebius Token Factory | Architecture & Active Params | Context | Role in `uta` |
|---|---|---|---|---|
| `ultra` | `nvidia/Nemotron-3-Ultra-550b-a55b` | Hybrid Mamba-2 + Transformer MoE (550B / 55B active) | 1M | **Planner, synthesizer, judge (the 10% tier)** — DAG decomposition, final synthesis, `judge … require k of n` verifiers, reflector critics |
| `super` | `nvidia/nemotron-3-super-120b-a12b` | Hybrid Mamba-2 + Transformer MoE (120B / 12B active) | 1M | **Worker swarm (the 90% tier)** — parallel coding subtasks with the agentic tool loop |
| `nano` | `nvidia/NVIDIA-Nemotron-3-Nano-30B-A3B` | Hybrid Mamba-2 + Transformer MoE (30B / 3B active) | 1M | **Compaction tier** — `--compact-over N` / `synthesis.compact_over` distils oversized worker outputs before Ultra synthesizes |
| `lightning` | `nvidia/Nemotron-3_5-Lightning` | Latent Mamba-Attention (30B / 3.5B active) | 256K | **Sentinel** — PASS/FAIL verdict on every `write_file`, `edit_file`, `run_command` before it executes (~300 ms measured) |
| `vl` | `nvidia/Nemotron-3.5-Super-VL` | Multimodal Vision-Language MoE (120B / 12B + ViT) | 512K | **Multimodal analyst** — diagrams and UI screenshots |

#### What the coordinator actually does

1. **Role-tier routing, executed by the engine.** Every provider call carries its orchestration role (planner, worker, synthesis, judge, compact, sentinel). With `--worker nebius` the Supervisor plans and synthesizes on **Nemotron 3 Ultra**, fans subtasks out to **Nemotron 3 Super**, and judges with Ultra — without any `[model:…]` tags in prompts. `NEBIUS_MODEL=<tier>` or an inline `[model:ultra]` pins a model; `UTA_NEBIUS_ROUTING=0` turns routing off. The trajectory's `session_id` events record `role` and `routing` so you can prove which tier ran each call.
2. **Agentic tool loop on a real repository.** With `--pre-approve "*"` (or `UTA_NEBIUS_AGENTIC=1`) Nemotron gets `read_file`, `write_file`, `edit_file`, `list_dir`, `run_command` and `tavily_search` as OpenAI-compatible function tools and iterates until the task is done; every call and result is recorded in SQLite.
3. **Nemotron Lightning sentinel.** Before any tool call that can change the workspace or run code, **Nemotron 3.5 Lightning** returns a PASS/FAIL verdict (reasoning off, temperature 0, 8 tokens). FAIL blocks the call, the agent sees the refusal as its tool result, and a `sentinel_alert` lands in the trajectory and in `uta dash`. Live on Token Factory a `curl … | sh` was blocked in 216 ms. `UTA_NEBIUS_SENTINEL=0` disables.
4. **Per-tier reasoning policy.** Nemotron 3 thinks by default; `uta` sets `reasoning_effort` per tier (Ultra high, Super medium, Nano low, Lightning none), keeps `reasoning_content` out of answers (recorded as a `reasoning` event), strips leaked `<think>` blocks and reports reasoning and prompt-cache tokens per turn. `UTA_NEBIUS_REASONING_EFFORT` overrides.
5. **Real dollar budgets and a cost report.** Built-in Nemotron list prices make `budget … $1` in steer and `--budget-usd` in `uta run` enforce on Nebius calls. `uta nebius cost <session>` folds a run into tokens and USD per tier and role and compares it with an all-Ultra counterfactual — a live coding run came out at $0.0123 vs $0.0271, a 54.7% saving.
6. **Robust on the shared endpoint.** Retries with backoff on 429/5xx (honouring `Retry-After`, `UTA_NEBIUS_MAX_RETRIES`), `NEBIUS_SERVICE_TIER` passthrough, and incremental SSE streaming so answers appear as Nemotron generates them.
7. **Keys and spend caps as first-class config.** `uta auth set nebius` stores the key in `~/.uta/credentials.env` (0600, outside every repo, prompted without echo). `uta limits set nebius --daily-usd 5 --monthly-usd 50` caps what the key may spend across every project on the machine; every call is ledgered and refused with a quota error once a cap is hit.
8. **Tavily grounding.** `uta nebius search` and the `tavily_search` tool call `POST https://api.tavily.com/search` live and persist citations to `.uta/context/RESEARCH_DATABASE.md`.
9. **Rules, skills and `@file` context.** `AGENTS.md`, `CLAUDE.md`, `GEMINI.md`, `.uta/rules/*.md`, `.agents/rules/*.md` and progressive `SKILL.md` skills are discovered and injected (`UTA_NEBIUS_INJECT_RULES=1`); `@path/to/file` mentions expand before the turn is sent.

```sh
uta auth set nebius                                   # store the Token Factory key (prompted, no echo)
uta auth set tavily                                   # optional: live web grounding
uta limits set nebius --daily-usd 5 --monthly-usd 50  # cap what the key may spend
uta nebius status                                     # endpoint, auth, routing rule, tiers, skills
uta nebius ping                                       # live connectivity against /v1/models
uta nebius models                                     # the five Nemotron tiers and aliases
uta nebius route "design a 90/10 multi-agent router with Nemotron"
uta nebius search "NVIDIA Nemotron 3 Mamba-2 MoE benchmarks"

uta run -g "add a table-driven test for internal/paths and run go test ./..." --worker nebius --pre-approve "*" -y
uta nebius cost <session-id>                          # tokens and USD per tier/role vs all-Ultra
uta run -f examples/hello-nemotron/uta.yaml -y        # four Nemotron tiers in parallel
uta run -g "<big task>" --worker nebius --compact-over 6000 -y   # Nano compacts long worker outputs before Ultra synthesizes
uta mission run examples/nebius-nemotron.steer        # steer mission on Nebius
uta bench                                             # latency, throughput and cost side by side
```

Environment knobs: `NEBIUS_API_KEY`, `NEBIUS_MODEL`, `NEBIUS_BASE_URL`, `NEBIUS_SYSTEM_PROMPT`, `NEBIUS_STREAM`, `NEBIUS_SERVICE_TIER`, `UTA_NEBIUS_ROUTING`, `UTA_NEBIUS_SENTINEL`, `UTA_NEBIUS_SENTINEL_MODEL`, `UTA_NEBIUS_REASONING_EFFORT`, `UTA_NEBIUS_MAX_TOKENS`, `UTA_NEBIUS_TEMPERATURE`, `UTA_NEBIUS_MAX_RETRIES`, `UTA_NEBIUS_AGENTIC`, `UTA_NEBIUS_INJECT_RULES`, `UTA_NEBIUS_PRICING`, `UTA_NEBIUS_{INPUT,OUTPUT}_CENTS_PER_KTOK`.

> **Nebius × NVIDIA Global AI Hackathon (Aug 26 – Oct 30, 2026) Updates:** Built the native `nebius` Token Factory provider for all 5 Nemotron tiers with role-tier routing executed by the engine, the agentic tool loop (`read_file`, `write_file`, `edit_file`, `list_dir`, `run_command`, `tavily_search`), the Nemotron Lightning sentinel, the per-tier reasoning policy, built-in list prices with enforced dollar budgets, `uta nebius` (`status`, `models`, `route`, `search`, `ping`, `cost`), `uta auth` and `uta limits`, retries and live streaming, Tavily grounding, unified `agy`/`claude` rules and skills discovery, shell slash commands and dashboard cards, and the `examples/hello-nemotron` sample project. See [`docs/DEVPOST-NEBIUS-HACKATHON.md`](docs/DEVPOST-NEBIUS-HACKATHON.md).

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
