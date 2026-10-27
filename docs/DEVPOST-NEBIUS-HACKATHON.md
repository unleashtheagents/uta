# Nebius × NVIDIA Global AI Hackathon — Devpost Submission Kit (`uta`)

- **Hackathon URL:** `https://nebiusglobalaihackathon.devpost.com/` (Devpost ID `30790`)
- **Deadline:** Friday, October 30, 2026 at 10:00 AM PDT (`18:00 CET`)
- **Public Repository:** `https://github.com/unleashtheagents/uta` (MIT License)
- **Live Website / Docs:** `https://unleashtheagents.ai`

---

## Step 1 — Project Overview

- **Project Name:** `uta-devpost-hackathon`
- **Elevator Pitch (max 200 chars):**
  `Open-source Go CLI & steer language that coordinates NVIDIA Nemotron 3 Ultra, Super, Nano, Lightning & Super-VL on Nebius Token Factory alongside Claude Code & Antigravity with Tavily search.` *(194 chars)*

---

## Step 2 — Project Details

### Recommended Track
- **Primary Track:** `Coding and agentic engineering` *(or `Best apps and agents`)*
  - *Why `Coding and agentic engineering`:* Matches the track's exact judge criteria: *"Build coding agents and developer tools: agents that write, run, and test code in Token Factory. Judges want an agent that plans, writes, tests, and iterates on a real repo with minimal human input."* Furthermore, since `Helixstreet — Nemotron Genome Lab` is in `Best apps and agents`, placing `uta` in `Coding and agentic engineering` maximizes coverage across both tracks.

### Built With (Tags)
`nebius-token-factory`, `nvidia-nemotron-3-ultra`, `nvidia-nemotron-3-super`, `nvidia-nemotron-3-nano`, `nvidia-nemotron-lightning`, `nvidia-nemotron-super-vl`, `tavily`, `go`, `sqlite`, `mcp`, `claude-code`, `google-antigravity`

### Try It Out Links
- `https://unleashtheagents.ai`
- `https://github.com/unleashtheagents/uta`
- `https://unleashtheagents.ai/uta/steer/`

### About the Project (Copy-Paste Markdown for Devpost)

```markdown
## Inspiration
A single coding-agent CLI (Claude Code, Google Antigravity `agy`, Gemini CLI) is great at one sequential conversation—but modern engineering tasks require parallel decomposition, cross-model verification, deterministic budget enforcement, and replayable audit trails. Worse, running every parallel worker subtask on a frontier closed model is slow and prohibitively expensive.

With **NVIDIA Nemotron 3** (`Ultra`, `Super`, `Nano`, `Lightning`, `Super-VL`) hosted on **Nebius Token Factory**, we realized we could implement a **90/10 Hybrid Multi-Agent Architecture**: routing 10% of tokens (DAG decomposition, architectural review, and final verdict synthesis) to **Nemotron 3 Ultra (550B/55B MoE)** while fanning out 90% of parallel coding, testing, and research subtasks to **Nemotron 3 Super (120B/12B MoE)** and **Nemotron 3 Nano (30B/3B MoE)** at 148–295 tok/s—supervised by **Nemotron 3.5 Lightning** for sub-50ms OWASP ASI01–ASI10 guardrails and grounded by **Tavily** live web search.

## What It Does
**`uta` (Unleash The Agents)** is a single-binary, zero-CGO Go CLI orchestrator, interactive shell, web dashboard, and **steer** programming language runtime that coordinates **NVIDIA Nemotron** models on **Nebius Token Factory (`https://api.tokenfactory.nebius.com/v1`)** alongside local agent CLIs (`claude`, `agy`, `gemini`):

1. **90/10 Nebius × Nemotron Coordinator (`uta nebius`):**
   - `uta nebius status` / `uta nebius ping` / `uta nebius models`: Inspects live Nebius Token Factory readiness across all 5 Nemotron tiers (`ultra`, `super`, `nano`, `lightning`, `vl`).
   - `uta nebius route "<goal>"`: Classifies any engineering goal into the optimal 90/10 Nemotron execution tier (`90/10-hybrid`, `ultra-deep`, `sentinel-fast`, `multimodal-vl`).
2. **Autonomous Agentic Tool-Calling Loop on Real Repositories (`internal/provider/builtin/nebius_tools.go`):**
   - Exposes OpenAI-compatible tools (`read_file`, `write_file`, `edit_file`, `list_dir`, `run_command`, `tavily_search`) directly to NVIDIA Nemotron models on Nebius Token Factory so Nemotron workers autonomously inspect code, edit files, run `go test ./...`, and iterate until verification gates pass.
3. **Runtime Web Grounding with Tavily (`uta nebius search` & `tavily_search`):**
   - Makes functional runtime calls to `POST https://api.tavily.com/search` (`internal/tavily`), synthesizes findings with NVIDIA Nemotron on Nebius Token Factory, and persists grounded citations to `.uta/context/RESEARCH_DATABASE.md`.
4. **Best-of-Breed `agy` + `claude` Features (`uta skills`, `@file`, `/plan`, `/compact`):**
   - Automatically discovers hierarchical workspace rules (`AGENTS.md`, `CLAUDE.md`, `GEMINI.md`, `.uta/rules/*.md`, `.agents/rules/*.md`) and progressive skills (`skills/<name>/SKILL.md`), expanding `@path/to/file` context mentions before dispatching prompts to Nebius Token Factory.
5. **Autonomous Pull Requests (`uta pr`), Multi-LLM Debates (`uta discuss`), and `steer` Language (`uta mission`):**
   - `uta pr -g "<goal>" --verify "go test ./..."` creates a branch, runs parallel Nemotron + CLI workers, enforces verification gates, and opens a GitHub PR.
   - `uta mission run examples/nebius-nemotron.steer` executes deterministic, budget-capped multi-agent programs with golden-journal replay (`uta mission test`).

## How We Built It
- **Language & Storage:** Pure Go (`CGO_ENABLED=0`) with embedded SQLite (`modernc.org/sqlite`) recording every prompt, SSE token chunk, tool call (`EventToolCall`), tool result (`EventToolResult`), token count, and USD cost in a queryable trajectory store.
- **Nebius Token Factory Provider (`internal/provider/builtin/nebius.go` & `nebius_tools.go`):** Native OpenAI-compatible client supporting both synchronous and SSE streaming completions, multi-turn session resumption, 5 Nemotron model aliases (`ultra`, `super`, `nano`, `lightning`, `vl`), `@file` expansion, progressive rule/skill injection, and autonomous tool execution.
- **Tavily Runtime Client (`internal/tavily/tavily.go`):** Native HTTP client for `POST https://api.tavily.com/search` wired into both `uta nebius search` and the `tavily_search` agentic function tool.

## Live Nebius Token Factory Benchmark Results (`uta bench`)

Measured live against `https://api.tokenfactory.nebius.com/v1`:

```text
$ uta nebius ping
Nebius Token Factory ping OK (88 ms) — 30 remote model(s) available

$ uta bench
PROVIDER     STATUS  DURATION  TOKENS_IN  TOKENS_OUT  THROUGHPUT
nebius       ok      2.016s    1264       271         134.4 tok/s
agy          ok      10.592s   15         0           -
claude       ok      11.099s   36391      536         48.3 tok/s
```

- **5.5× faster wall-clock completion** (`2.016s` on Nebius Token Factory vs `11.099s` on Claude Code CLI and `10.592s` on Google Antigravity CLI).
- **2.78× higher output token throughput** (`134.4 tok/s` vs `48.3 tok/s`).
- **4-second autonomous tool-calling loop (`session 834eefb7`)**: `nvidia/Nemotron-3-Ultra-550b-a55b` decomposed the goal, invoked the `read_file` tool on `examples/nebius-nemotron.steer`, verified the result, and synthesized the final report in **4s (4,953 tokens)**.
```

---

## Step 3 — Additional Info (Required Questions & Answers)

### 1. Existing Project / Updates Made During the Submission Period (Aug 26 – Oct 30, 2026)
> **Copy-Paste Answer:**
> During the Nebius × NVIDIA Global AI Hackathon submission window (Aug 26 – Oct 30, 2026), we built and integrated the complete Nebius Token Factory × NVIDIA Nemotron coordination stack in `uta` and `unleashtheagents.ai`:
> 1. **Built-in `nebius` Provider (`internal/provider/builtin/nebius.go` & `nebius_tools.go`):** Added native Nebius Token Factory (`https://api.tokenfactory.nebius.com/v1`) support for all 5 NVIDIA Nemotron tiers (`nvidia/Nemotron-3-Ultra-550b-a55b`, `nvidia/nemotron-3-super-120b-a12b`, `nvidia/Nemotron-3-Nano-30b-a3b`, `nvidia/Nemotron-3_5-Lightning`, `nvidia/Nemotron-3.5-Super-VL`), SSE token streaming, live `/v1/models` probing, and a multi-turn agentic tool-calling loop (`read_file`, `write_file`, `edit_file`, `list_dir`, `run_command`, `tavily_search`).
> 2. **`uta nebius` Coordinator CLI (`internal/cli/nebius.go`):** Added `uta nebius status`, `uta nebius models`, `uta nebius route <goal>`, `uta nebius search <query>`, and `uta nebius ping`.
> 3. **Runtime Tavily Search Integration (`internal/tavily/tavily.go`):** Added functional runtime calls to `POST https://api.tavily.com/search` for both CLI research (`uta nebius search`) and autonomous agent tool calls (`tavily_search`), with automatic synthesis via NVIDIA Nemotron and persistence to `.uta/context/RESEARCH_DATABASE.md`.
> 4. **Unified `agy` + `claude` Customization Engine (`internal/skills/skills.go`, `internal/cli/skills.go`):** Added hierarchical discovery of `AGENTS.md`, `CLAUDE.md`, `GEMINI.md`, `.uta/rules/*.md`, `.agents/rules/*.md`, and progressive `SKILL.md` skills (`uta skills list`, `uta skills show`), plus `@path/to/file` context expansion for cloud Nemotron runs.
> 5. **Interactive Shell, Web Dashboard & `steer` Mission (`internal/cli/shell.go`, `internal/cli/serve_web.go`, `examples/nebius-nemotron.steer`):** Added `/plan`, `/compact`, `/nebius`, `/skills` slash commands, `/api/nebius` & `/api/skills` web dashboard endpoints, and deterministic `steer` missions for Nebius × Nemotron.

### 2. Best Use of Tavily Bonus Award ($3,000)
- **Did you integrate Tavily into your submission?** `Yes`
  - *Verification:* `uta` makes functional runtime calls to `POST https://api.tavily.com/search` via `internal/tavily/tavily.go` when running `uta nebius search "<query>"` and when NVIDIA Nemotron invokes the `tavily_search` tool during `uta run --worker nebius -y`.
  - *Live Recorded SQLite Trajectory (`session 29643605`, 10s, 9,627 tokens):* `nvidia/Nemotron-3-Ultra-550b-a55b` autonomously chained two live `tavily_search` tool calls (`"NVIDIA Nemotron 3 arxiv 2512.20856"` and `"Nemotron 3 throughput improvement compared to previous models"`), grounded its reasoning in `https://arxiv.org/html/2512.20856v1` (score 0.91), and persisted the synthesized report to `.uta/context/RESEARCH_DATABASE.md`:
    ```text
    11:26:14  subtask_started         deb516c4  title=Search NVIDIA Nemotron 3 arxiv 2512.20856 and summarize throughput improvement
    11:26:14  subtask_tool_call       deb516c4  {"arguments":"{\"query\": \"NVIDIA Nemotron 3 arxiv 2512.20856\"}","tool_name":"tavily_search"}
    11:26:17  subtask_tool_result     deb516c4  {"is_error":false,"output":"### Tavily Live Search: NVIDIA Nemotron 3 arxiv 2512.20856..."}
    11:26:17  subtask_tool_call       deb516c4  {"arguments":"{\"query\": \"Nemotron 3 throughput improvement compared to previous models\"}","tool_name":"tavily_search"}
    11:26:19  subtask_tool_result     deb516c4  {"is_error":false,"output":"### Tavily Live Search: Nemotron 3 throughput improvement..."}
    11:26:22  run_completed           -         status=completed (duration=10s, tokens=9627)
    ```

### 3. Ten Required Nemotron & Nebius Token Factory Feedback Questions (Most Valuable Feedback Award)

1. **Which Nemotron models did you use in your submission?**
   - `Nemotron 3 Ultra` (`nvidia/Nemotron-3-Ultra-550b-a55b`)
   - `Nemotron 3 Super` (`nvidia/nemotron-3-super-120b-a12b`)
   - `Nemotron 3 Nano` (`nvidia/Nemotron-3-Nano-30b-a3b`)
   - `Other`: `nvidia/Nemotron-3_5-Lightning` and `nvidia/Nemotron-3.5-Super-VL`

2. **How did you run the Nemotron models?**
   - `Nebius Token Factory` (`https://api.tokenfactory.nebius.com/v1`)

3. **How would you rate the output quality of the Nemotron models for your use case?**
   - `5 — Excellent`

4. **How did Nemotron's speed and latency feel during your workflow?**
   - `5 — Very fast, exceeded expectations`

5. **How easy was it to get started with Nemotron on Nebius?**
   - `5 — Very easy, up and running quickly`

6. **How did Nemotron compare to other models you've used for similar tasks?**
   - `Much better`

7. **What worked well with Nemotron and Nebius Token Factory?**
   > The OpenAI-compatible `/v1/chat/completions` and `/v1/models` endpoints on Nebius Token Factory (88 ms ping latency across 30 hosted models) allowed us to build a zero-dependency Go provider (`internal/provider/builtin/nebius.go`) with SSE streaming and multi-turn function calling (`tools` / `tool_calls`) in hours. In live side-by-side benchmarks (`uta bench`), Nebius Token Factory completed turns in **2.016s at 134.4 tok/s**—**5.5× faster in wall-clock latency and 2.78× higher in token throughput** than local CLI cold starts (`claude` at 11.099s / 48.3 tok/s, `agy` at 10.592s). Furthermore, `nvidia/Nemotron-3-Ultra-550b-a55b` executed a complete multi-turn agentic `read_file` tool loop + synthesis in just **4 seconds** (`session 834eefb7`), while `nvidia/NVIDIA-Nemotron-3-Nano-30B-A3B` completed 3-stage plan + worker + synthesis runs in **7 seconds** (`session 9d6a5b0e`).

8. **What was frustrating, broken, or harder than expected?**
   > 1. Model ID casing and prefixes across the live `/v1/models` catalog vary slightly (`nvidia/Nemotron-3-Ultra-550b-a55b` vs lowercase `nvidia/nemotron-3-super-120b-a12b` vs `nvidia/NVIDIA-Nemotron-3-Nano-30B-A3B` with the `NVIDIA-` prefix vs underscore `nvidia/Nemotron-3_5-Lightning`), which can trip up developers copying model IDs from docs. We solved this in `uta` by building a canonical alias resolver (`ResolveNebiusModel`: `ultra`, `super`, `nano`, `lightning`, `vl`) that normalizes all variants and live `/v1/models` probing (`uta nebius ping`).
   > 2. When streaming (`stream: true`) is combined with multi-turn tool calls (`tools`), chunked tool argument deltas vary slightly across model families; exposing a standard `/v1/models` capability flag indicating `supports_tool_calling` and `supports_reasoning_effort` per model ID on Nebius Token Factory would make orchestrator auto-configuration even smoother.

9. **Did you use any other Nebius services beyond Token Factory? If so, which ones and how was the experience?**
   > We focused primarily on Nebius Token Factory (`https://api.tokenfactory.nebius.com/v1`) as the cloud inference backbone for `uta`'s 90/10 multi-agent coordinator, pairing it with local developer workspaces and SQLite trajectory recording so any developer can install `uta` locally and immediately burst parallel Nemotron worker swarms to Nebius Token Factory.

10. **Would you continue using Nemotron models and Nebius after the hackathon?**
    - `Yes, definitely`
