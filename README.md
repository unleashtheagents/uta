# uta — unleash the agents

[![ci](https://github.com/unleashtheagents/uta/actions/workflows/ci.yml/badge.svg)](https://github.com/unleashtheagents/uta/actions/workflows/ci.yml) [![release](https://github.com/unleashtheagents/uta/actions/workflows/release.yml/badge.svg)](https://github.com/unleashtheagents/uta/actions/workflows/release.yml) [![install channels](https://github.com/unleashtheagents/uta/actions/workflows/install-channels.yml/badge.svg)](https://github.com/unleashtheagents/uta/actions/workflows/install-channels.yml)

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

## First run

```sh
uta                      # in a terminal: first-run setup if nothing is configured, then the interactive shell
uta setup                # paste a Nebius Token Factory key, pick a daily spend cap, optional Tavily key, live ping
uta nebius demo          # 90 seconds: Ultra plans, Super codes + tests, Tavily grounds, Lightning blocks, Nano compacts, cost report
uta run -g "summarize this repo in 5 bullets"         # no flags: worker auto-selected (nebius if a key exists), read-only tools, cost footer
```

Every run ends with a one-line footer such as `[uta] nebius: ultra 2 calls $0.0015 · super 1 call $0.0008 · saved 45% vs all-Ultra`.

## 60-second tour

```sh
uta hello                                             # what is this, what's installed, what to try next
uta doctor                                            # verify environment
uta providers                                         # list detected agent CLIs (claude, agy, etc.)
uta run -g "summarize this repo in 5 bullets"         # ad-hoc orchestration (worker auto-selected; -y only for scripts)
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
uta run -f examples/workflows/wordlab.yaml -y         # DAG demo: parallel Nemotron Super workers + go test gates build a tool
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
2. **Agentic tool loop on a real repository.** Nemotron gets nine tools: `read_file`, `list_dir`, `search_files` (regex, like `grep -rn`), `glob` (`**/*.go`), `tavily_search`, `tavily_extract` (read a page in full), `write_file`, `edit_file` (unique-match replace, refuses ambiguous targets) and `run_command`. Read-only tools run without asking; the rest go through the permissions gate below and the Lightning sentinel. Every call, result and file diff is recorded in SQLite, and the progress view shows each tool call and a colored diff of each edit as it happens.
3. **Nemotron Lightning sentinel, tiered by blast radius.** Before any tool call that can change the workspace or run code, **Nemotron 3.5 Lightning** returns a PASS/FAIL verdict (reasoning off, temperature 0, 8 tokens, ~200 ms). A FAIL hard-blocks `run_command` — the agent sees the refusal as its tool result and a `sentinel_alert` lands in the trajectory and `uta dash`; live on Token Factory a piped-download install was blocked in 216 ms. On `write_file` and `edit_file`, which are path-confined and checkpointed (one `uta undo` reverts them), a FAIL warns instead of blocking, so an uncertain verdict never stalls ordinary editing; `UTA_NEBIUS_SENTINEL_STRICT=1` restores hard blocking and `UTA_NEBIUS_SENTINEL=0` disables.
4. **Per-tier reasoning policy.** Nemotron 3 thinks by default; `uta` sets `reasoning_effort` per tier (Ultra high, Super medium, Nano low, Lightning none), keeps `reasoning_content` out of answers (recorded as a `reasoning` event), strips leaked `<think>` blocks and reports reasoning and prompt-cache tokens per turn. `UTA_NEBIUS_REASONING_EFFORT` overrides.
5. **Real dollar budgets and a cost report.** Built-in Nemotron list prices make `budget … $1` in steer and `--budget-usd` in `uta run` enforce on Nebius calls. `uta nebius cost <session>` folds a run into tokens and USD per tier and role and compares it with an all-Ultra counterfactual — a live coding run came out at $0.0123 vs $0.0271, a 54.7% saving.
6. **Robust on the shared endpoint.** Retries with backoff on 429/5xx (honouring `Retry-After`, `UTA_NEBIUS_MAX_RETRIES`), `NEBIUS_SERVICE_TIER` passthrough, and incremental SSE streaming so answers appear as Nemotron generates them.
7. **Keys and spend caps as first-class config.** `uta auth set nebius` stores the key in `~/.uta/credentials.env` (0600, outside every repo, prompted without echo). `uta limits set nebius --daily-usd 5 --monthly-usd 50` caps what the key may spend across every project on the machine; every call is ledgered and refused with a quota error once a cap is hit.
8. **Tavily grounding.** `uta nebius search` and the `tavily_search` tool call `POST https://api.tavily.com/search` live; both persist citations to `.uta/context/RESEARCH_DATABASE.md`, and the cost report and run footer count Tavily searches per run.
9. **Rules, skills and `@file` context.** `AGENTS.md`, `CLAUDE.md`, `GEMINI.md`, `.uta/rules/*.md`, `.agents/rules/*.md` and progressive `SKILL.md` skills are discovered and injected (`UTA_NEBIUS_INJECT_RULES=1`); `@path/to/file` and `@dir/` mentions attach the file or listing before the turn is sent, in the shell for every worker.
10. **Permissions, prompts and hooks.** `.uta/permissions.yaml` (per repo) and `~/.uta/permissions.yaml` (global) hold `allow` / `ask` / `deny` patterns in the `tool(glob)` grammar uta already uses for mission profiles: `run_command(go test *)`, `write_file(.env*)`. Deny beats ask beats allow; unmatched write/edit/run calls ask. In a terminal the prompt offers **[y] once, [s] this run, [a] always** (which saves the rule); piped runs deny with the exact `uta permissions allow '...'` to add. `pre_tool` hooks can block a call (exit 2) and `post_tool` hooks can push lint or test feedback back to the model (exit 2). `uta permissions init|status|allow|ask|deny`.
11. **Checkpoints and undo.** Before any `write_file` or `edit_file` the gate snapshots the file into the content-addressed blob store and records a checkpoint on the session. `uta undo [session]` restores every touched file (new files removed), `uta undo --diff` shows the net change, `--dry-run` and `--list` preview. In the shell: `/diff`, `/rewind`.
12. **Vision with Nemotron Super-VL.** `uta nebius see shot.png "what is wrong here"` and the shell's `/image` send a screenshot, diagram or photo to **Nemotron 3.5 Super-VL**; the shell attaches the answer to your next goal, so "fix what this screenshot shows" works. Counted against the daily cap like every other call.
13. **MCP tools for Nemotron.** Drop a `.mcp.json` in the repo (the file Claude Code and Cursor already use) or list `mcp_servers` in a mission profile, and every server's tools are advertised to Nemotron as `mcp__<server>__<tool>`, launched once per process over stdio, and run through the same permissions gate and sentinel as the built-in tools. One rule such as `mcp__github__create_issue` covers both the Nemotron path and the `claude` CLI path. `UTA_NEBIUS_MCP=0` turns it off.
14. **Direct mode and conversation compaction.** `uta run --direct` and the shell's `/direct` skip planner and synthesis: the goal goes to one Nemotron Super worker with the full tool set and the conversation continues turn by turn, the way `claude` and `agy` work. When the history grows past about 40k tokens, **Nemotron 3 Nano** compacts the older part into a summary and the worker keeps reading a short context (`UTA_NEBIUS_COMPACT_HISTORY_TOKENS`). `/fanout <goal>` still orchestrates one goal with planner and parallel workers when the task is big.

```sh
uta auth set nebius                                   # store the Token Factory key (prompted, no echo)
uta auth set tavily                                   # optional: live web grounding
uta limits set nebius --daily-usd 5 --monthly-usd 50  # cap what the key may spend
uta nebius status                                     # endpoint, auth, routing rule, tiers, skills
uta nebius ping                                       # live connectivity against /v1/models
uta nebius models                                     # the five Nemotron tiers and aliases
uta nebius route "design a 90/10 multi-agent router with Nemotron"
uta nebius search "NVIDIA Nemotron 3 Mamba-2 MoE benchmarks"

uta run -g "add a table-driven test for internal/paths and run go test ./..." --worker nebius   # asks before each write / command
uta permissions init                                  # starter .uta/permissions.yaml: allow go test, deny rm -rf, hooks
uta permissions allow 'run_command(go test *)'        # or answer [a]lways at the prompt
uta undo --diff                                       # what the last run changed; uta undo restores it
uta nebius see shot.png "what is broken in this UI?"  # Nemotron Super-VL reads the image
uta nebius extract https://docs.nebius.com/...        # Tavily Extract: read a page in full
uta run --direct -g "fix the failing test in pkg/x" --worker nebius   # one Super worker with tools, no planner
uta shell --direct                                    # claude/agy-style conversation on Nemotron; /fanout for big goals
uta nebius cost <session-id>                          # tokens and USD per tier/role vs all-Ultra
uta run -f examples/hello-nemotron/uta.yaml -y        # four Nemotron tiers in parallel
uta run -g "<big task>" --worker nebius --compact-over 6000 -y   # Nano compacts long worker outputs before Ultra synthesizes
uta mission run examples/nebius-nemotron.steer        # steer mission on Nebius
uta bench                                             # latency, throughput and cost side by side
```

Environment knobs: `NEBIUS_API_KEY`, `NEBIUS_MODEL`, `NEBIUS_BASE_URL`, `NEBIUS_SYSTEM_PROMPT`, `NEBIUS_STREAM`, `NEBIUS_SERVICE_TIER`, `UTA_NEBIUS_ROUTING`, `UTA_NEBIUS_SENTINEL`, `UTA_NEBIUS_SENTINEL_MODEL`, `UTA_NEBIUS_REASONING_EFFORT`, `UTA_NEBIUS_MAX_TOKENS`, `UTA_NEBIUS_TEMPERATURE`, `UTA_NEBIUS_MAX_RETRIES`, `UTA_NEBIUS_AGENTIC`, `UTA_NEBIUS_INJECT_RULES`, `UTA_NEBIUS_PRICING`, `UTA_NEBIUS_{INPUT,OUTPUT}_CENTS_PER_KTOK`, `UTA_NEBIUS_VISION_MODEL`, `UTA_NEBIUS_MCP`, `UTA_NEBIUS_COMPACT_HISTORY_TOKENS`, `UTA_TOOL_PROMPT=0` (never prompt; ask rules deny).

> **Nebius × NVIDIA Global AI Hackathon (Aug 26 – Oct 30, 2026) Updates:** Built the native `nebius` Token Factory provider for all 5 Nemotron tiers with role-tier routing executed by the engine, the agentic tool loop (`read_file`, `list_dir`, `search_files`, `glob`, `write_file`, `edit_file`, `run_command`, `tavily_search`, `tavily_extract`), the permissions gate (`.uta/permissions.yaml`, interactive allow/always prompts, pre/post hooks), file checkpoints with `uta undo`, Nemotron Super-VL vision (`uta nebius see`, `/image`), MCP tools for Nemotron from `.mcp.json`, direct mode with Nano conversation compaction, the Nemotron Lightning sentinel, the per-tier reasoning policy, built-in list prices with enforced dollar budgets, `uta nebius` (`status`, `models`, `route`, `search`, `ping`, `cost`), `uta auth` and `uta limits`, retries and live streaming, Tavily grounding, unified `agy`/`claude` rules and skills discovery, shell slash commands and dashboard cards, and the `examples/hello-nemotron` sample project. See [`docs/DEVPOST-NEBIUS-HACKATHON.md`](docs/DEVPOST-NEBIUS-HACKATHON.md).

`uta pr` runs an autonomous dev mission on a dedicated Git branch (`uta/<slug>`), validates it against verification gates, commits changes, and opens a Pull Request via `gh pr create` with a structured trajectory summary.

`uta serve --web` launches an interactive local Web Dashboard HTTP server (`http://127.0.0.1:8080`) providing real-time stats, session tracking, and provider status.

`uta discuss` pits two agents — ideally two different LLMs (claude vs
agy) — against each other for several rounds of rebuttals, then a
moderator synthesizes agreements, disagreements, and a verdict. See
[`docs/discussion.md`](docs/discussion.md).

`uta shell` is a persistent, terminal-style prompt on top of the same
engine: type a goal and it fans out; type again and the conversation
continues. Its slash commands are a superset of what the underlying agent
CLIs offer — `/model`, `/tools`, `/clear`, `@file` mentions, `/image`,
`/diff`, `/rewind` and `/direct` work like in claude/agy, plus uta-level control like
`/worker`, `/mode`, `/agents`, `/cost`, `/permissions`, and `@agy <goal>`
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
