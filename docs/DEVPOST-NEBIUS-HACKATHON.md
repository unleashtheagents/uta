# Nebius × NVIDIA Global AI Hackathon — Devpost Submission Kit (`uta`)

- **Hackathon URL:** `https://nebiusglobalaihackathon.devpost.com/` (Devpost ID `30790`)
- **Deadline:** Friday, October 30, 2026 at 10:00 AM PDT (`18:00 CET`)
- **Public Repository:** `https://github.com/unleashtheagents/uta` (MIT License)
- **Live Website / Docs:** `https://unleashtheagents.ai`

---

## Step 1 — Project Overview

- **Project Name:** `uta-devpost-hackathon`
- **Elevator Pitch (max 200 chars):**
  `Open-source Go orchestrator + steer language: Nemotron Ultra plans, Super swarms, Lightning guards every tool call on Nebius Token Factory — with real budgets, spend caps and Tavily grounding.` *(196 chars)*

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

With **NVIDIA Nemotron 3** (`Ultra`, `Super`, `Nano`, `Lightning`, `Super-VL`) hosted on **Nebius Token Factory**, we realized we could implement a **90/10 Hybrid Multi-Agent Architecture**: routing 10% of tokens (DAG decomposition, architectural review, and final verdict synthesis) to **Nemotron 3 Ultra (550B/55B MoE)** while fanning out 90% of parallel coding, testing, and research subtasks to **Nemotron 3 Super (120B/12B MoE)** and **Nemotron 3 Nano (30B/3B MoE)** at 148–295 tok/s—supervised by **Nemotron 3.5 Lightning**, which screens every workspace-changing tool call with a ~300 ms OWASP ASI01–ASI10 PASS/FAIL verdict, and grounded by **Tavily** live web search.

## What It Does
**`uta` (Unleash The Agents)** is a single-binary, zero-CGO Go CLI orchestrator, interactive shell, web dashboard, and **steer** programming language runtime that coordinates **NVIDIA Nemotron** models on **Nebius Token Factory (`https://api.tokenfactory.nebius.com/v1`)** alongside local agent CLIs (`claude`, `agy`, `gemini`):

1. **90/10 Nebius × Nemotron Coordinator, executed by the engine (`uta nebius`, `internal/engine`):**
   - Every provider call carries its orchestration role (planner, worker, synthesis, judge, compact, sentinel). With `--worker nebius` the Supervisor plans and synthesizes on **Nemotron 3 Ultra**, fans subtasks out to **Nemotron 3 Super**, judges with Ultra, compacts with **Nemotron 3 Nano** and guards with **Nemotron 3.5 Lightning** — no prompt tags required. Each trajectory `session_id` event records `role` and `routing` as proof.
   - **First run:** a bare `uta` in a terminal runs `uta setup` when nothing is configured (Nebius key, daily spend cap, optional Tavily key, live ping) and opens the shell; `uta run -g "..."` needs no flags (worker auto-selected, read-only tools, cost footer); `uta nebius demo` is a 90-second tour that exercises every tier plus Tavily and ends with the cost report.
   - `uta nebius status` / `ping` / `models` / `route "<goal>"` inspect readiness, the routing rule and the five tiers; `uta nebius cost <session>` reports tokens and USD per tier and role against an all-Ultra counterfactual (live coding run: **$0.0123 vs $0.0271, a 54.7% saving**).
   - **Nemotron Lightning sentinel, tiered by blast radius:** before any `write_file`, `edit_file` or `run_command` executes, Lightning returns PASS/FAIL (reasoning off, temperature 0, 8 tokens, ~200 ms). FAIL hard-blocks `run_command` (live: a piped-download install blocked in **216 ms**); on the checkpointed file tools it warns and lets `uta undo` be the safety net (`UTA_NEBIUS_SENTINEL_STRICT=1` restores blocking). A 21-case live consistency run after tuning: every dangerous call failed and every benign command and write passed; the only residual FAILs were on a test-fixture edit, where the tiered design warns instead of blocking.
   - **Per-tier reasoning policy:** `reasoning_effort` high/medium/low/none per tier, `reasoning_content` kept out of answers and recorded separately, reasoning and prompt-cache tokens reported per turn.
   - **Real budgets, keys and caps:** built-in Nemotron list prices make `budget … $1` and `--budget-usd` enforce; `uta auth set nebius` stores keys outside every repo; `uta limits set nebius --daily-usd 5` caps spend per provider across all projects, ledgered on every call.
2. **Autonomous Agentic Tool-Calling Loop on Real Repositories (`internal/provider/builtin/nebius_tools.go`):**
   - Exposes nine OpenAI-compatible tools (`read_file`, `list_dir`, `search_files`, `glob`, `write_file`, `edit_file`, `run_command`, `tavily_search`, `tavily_extract`) directly to NVIDIA Nemotron models on Nebius Token Factory so Nemotron workers find code with regex and glob search, edit files with unique-match replacement, run `go test ./...`, read web pages in full, and iterate until verification gates pass. Every edit is recorded with a unified diff and shown live in the progress view.
   - **Permissions gate, prompts and hooks (`internal/policy`, `internal/cli/toolgate.go`):** `.uta/permissions.yaml` holds `allow` / `ask` / `deny` patterns (`run_command(go test *)`, `write_file(.env*)`); unmatched write/edit/run calls ask in the terminal with **[y] once / [s] this run / [a] always** and deny with an exact fix hint when piped. `pre_tool` hooks block (exit 2), `post_tool` hooks feed lint or test output back to Nemotron. The Lightning sentinel runs after the gate, so policy, human and model guard every call in that order.
   - **Checkpoints and undo (`file_checkpoints`, `uta undo`, `/rewind`):** every `write_file` / `edit_file` first snapshots the file into the content-addressed blob store; `uta undo --diff` shows the net change of a run and `uta undo` restores it.
   - **Vision (`uta nebius see`, `/image`):** Nemotron 3.5 Super-VL reads screenshots and diagrams; the shell attaches the answer to the next goal.
   - **MCP tools for Nemotron (`nebius_mcp.go`):** servers from the repo's `.mcp.json` or the mission profile are launched over stdio and their tools advertised as `mcp__<server>__<tool>`, gated like every other tool. A live run called a stub server's `echo` tool from a 3-second, one-call direct turn.
   - **Direct mode (`uta run --direct`, shell `/direct`):** no planner, no synthesis; one Nemotron Super worker with tools and a continuing conversation. Nemotron 3 Nano compacts the history past ~40k tokens (`history_compacted` event), so long sessions stay cheap. `/fanout` escalates one goal to the full planner.
3. **Runtime Web Grounding with Tavily (`uta nebius search`, `uta nebius extract`, `tavily_search`, `tavily_extract`):**
   - Makes functional runtime calls to `POST https://api.tavily.com/search` and `POST https://api.tavily.com/extract` (`internal/tavily`), synthesizes findings with NVIDIA Nemotron on Nebius Token Factory, and persists grounded citations to `.uta/context/RESEARCH_DATABASE.md`.
4. **Best-of-Breed `agy` + `claude` Features (`uta skills`, `@file`, `/image`, `/diff`, `/rewind`, `/plan`, `/compact`):**
   - Automatically discovers hierarchical workspace rules (`AGENTS.md`, `CLAUDE.md`, `GEMINI.md`, `.uta/rules/*.md`, `.agents/rules/*.md`) and progressive skills (`skills/<name>/SKILL.md`); `@path/to/file` and `@dir/` mentions attach context in the shell for every worker.
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
> 6. **Role-Tier Routing in the Engine (`internal/engine/supervisor.go`, `provider.RunOptions.Role`):** Planner, synthesis and judge calls run on Nemotron 3 Ultra, workers on Super, compaction on Nano, sentinel checks on Lightning — recorded per call in the trajectory.
> 7. **Nemotron Lightning Sentinel (`internal/provider/builtin/nebius_sentinel.go`):** PASS/FAIL guardrail on every `write_file` / `edit_file` / `run_command`, blocking FAILs and persisting `sentinel_alert` events (live block in 216 ms).
> 8. **Reasoning Policy, Pricing and Cost Report (`nebius.go`, `pricing.go`, `internal/cli/nebius_cost.go`):** per-tier `reasoning_effort`, separated `reasoning_content`, built-in Nemotron list prices so dollar budgets enforce, `uta nebius cost <session>` with the all-Ultra counterfactual.
> 9. **Keys and Spend Caps (`internal/cli/auth.go`, `internal/limits`, `internal/cli/limits.go`):** `uta auth set|unset|status` and `uta limits set|clear|status` with a per-call usage ledger enforced before every provider call.
> 11. **First-run experience (`internal/cli/setup.go`, `nebius_demo.go`):** bare `uta` opens the shell with a first-run wizard, zero-flag `uta run`, `uta nebius demo`, per-tier cost footer on every run, Tavily searches counted per run and citations persisted from agent tool calls.
> 10. **Robustness (`nebius.go`):** retries with backoff on 429/5xx honouring `Retry-After`, `service_tier` passthrough, incremental SSE streaming with coalesced deltas, `stream_options.include_usage`.
> 12. **Permissions, hooks, checkpoints (`internal/policy`, `internal/cli/toolgate.go`, `internal/cli/undo.go`, migration `0009_file_checkpoints`):** `.uta/permissions.yaml` allow/ask/deny rules with interactive once/run/always prompts, `pre_tool` / `post_tool` shell hooks, `uta permissions`, file checkpoints before every write/edit, `uta undo [--diff|--dry-run|--list]`, shell `/diff`, `/rewind`, `/cost`, `/permissions`.
> 14. **MCP client and direct mode (`nebius_mcp.go`, `nebius_history.go`, `internal/cli/shell.go`):** `.mcp.json` / profile MCP servers as native Nemotron tools (`mcp__server__tool`, launched once per process, gated and sentinel-checked), `uta run --direct` and `/direct` single-worker conversations with Nano history compaction, `/fanout` escalation.
> 13. **Code search, page reading and vision (`nebius_tools.go`, `nebius_vision.go`, `internal/tavily`):** `search_files` and `glob` tools, `tavily_extract` tool plus `uta nebius extract`, unique-match `edit_file`, live tool-call and diff rendering in the progress view, Nemotron 3.5 Super-VL via `uta nebius see` and the shell's `/image` and `@file` / `@dir/` attachments.

### 2. Best Use of Tavily Bonus Award ($3,000)
- **Did you integrate Tavily into your submission?** `Yes`
  - *Verification:* `uta` makes functional runtime calls to `POST https://api.tavily.com/search` and `POST https://api.tavily.com/extract` via `internal/tavily/tavily.go` when running `uta nebius search "<query>"` / `uta nebius extract <url>` and when NVIDIA Nemotron invokes the `tavily_search` and `tavily_extract` tools during `uta run --worker nebius` (both are in the default read-only tool set, so no flags are needed).
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

> All numbers below were measured on 2026-10-27 against `https://api.tokenfactory.nebius.com/v1` from uta's own trajectories.

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
   - `4 — Fast`. Measured: `/v1/models` 88–101 ms; Lightning PASS/FAIL verdict 216–450 ms (293 ms with reasoning disabled); a Super worker turn 1.0 s; a full Ultra-plan → Super-work → Ultra-synthesize coding run 17 s. Fast enough to put a guardrail model in the tool loop, which is the point.

5. **How easy was it to get started with Nemotron on Nebius?**
   - `5 — Very easy, up and running quickly`

6. **How did Nemotron compare to other models you've used for similar tasks?**
   - `Much better`

7. **What worked well with Nemotron and Nebius Token Factory?**
   > The OpenAI-compatible `/v1/chat/completions` and `/v1/models` endpoints on Nebius Token Factory (88 ms ping latency across 30 hosted models) allowed us to build a zero-dependency Go provider (`internal/provider/builtin/nebius.go`) with SSE streaming and multi-turn function calling (`tools` / `tool_calls`) in hours. In live side-by-side benchmarks (`uta bench`), Nebius Token Factory completed turns in **2.016s at 134.4 tok/s**—**5.5× faster in wall-clock latency and 2.78× higher in token throughput** than local CLI cold starts (`claude` at 11.099s / 48.3 tok/s, `agy` at 10.592s). Furthermore, `nvidia/Nemotron-3-Ultra-550b-a55b` executed a complete multi-turn agentic `read_file` tool loop + synthesis in just **4 seconds** (`session 834eefb7`), while `nvidia/NVIDIA-Nemotron-3-Nano-30B-A3B` completed 3-stage plan + worker + synthesis runs in **7 seconds** (`session 9d6a5b0e`).

8. **What was frustrating, broken, or harder than expected?**
   > 1. **Thinking leaks into `content` by default.** Nemotron 3 reasons unless told otherwise. With no `reasoning_effort`, `nvidia/Nemotron-3_5-Lightning` spent its entire 64-token budget on chain-of-thought and returned it *as the answer* (`"Here's a thinking process: 1. Analyze User Input…"`), while `nemotron-3-super` put the same thinking in `reasoning_content` and answered `"\n\nPASS"`. The fix (`reasoning_effort: "none"`, or `chat_template_kwargs.enable_thinking=false`) works but is not in the Nebius chat-completion reference, and the two models behave differently with the same request. A per-model default and a documented toggle would save every integrator a day.
   > 2. **`max_tokens` counts reasoning.** The documented default of 8192 includes `reasoning_tokens`, so a short-answer request with reasoning on can return an empty `content`. Surfacing `reasoning_tokens` in `usage.completion_tokens_details` (which Nebius does) is great; a `max_reasoning_tokens` knob would complete it.
   > 3. **No capability flags on `/v1/models`.** Nothing tells a client which IDs support tools, reasoning_effort, vision or streaming-with-tools; we hard-code a catalog and probe. `supports_tools` / `supports_reasoning` / `modalities` fields per model would let orchestrators auto-configure.
   > 4. **Model ID conventions vary** across the catalog (`Nemotron-3-Ultra-550b-a55b`, `nemotron-3-super-120b-a12b`, `NVIDIA-Nemotron-3-Nano-30B-A3B`, `Nemotron-3_5-Lightning`, `Nemotron-3.5-Super-VL`). We ship an alias resolver (`ultra|super|nano|lightning|vl`); canonical lowercase IDs with stable aliases server-side would be better.
   > 5. **Sandboxes access path is unclear for Token Factory API-key users.** The coding track names Token Factory Sandboxes, the docs say the Token Factory key plus a Project ID is enough, yet `GET /sandboxes/v1/auth` returned 404 for us and the overview says access must be requested. A self-serve toggle in the console would have let us run `go test` inside a Sandbox microVM instead of on the host.
   > 6. **Pricing is not on docs.tokenfactory.nebius.com.** We had to take Nemotron per-token prices from third-party listings to make dollar budgets enforce; a `/v1/models` `pricing` field or a documented page would make cost accounting exact.

9. **Did you use any other Nebius services beyond Token Factory? If so, which ones and how was the experience?**
   > Token Factory inference only. We designed `run_command` as a pluggable execution backend so Token Factory **Sandboxes** can run each worker's `go test` in a disposable microVM (one prepared image, one branch per parallel worker, `judge … require k of n` keeps the passing branch), but could not obtain Sandboxes access during the submission window — see question 8.

10. **Would you continue using Nemotron models and Nebius after the hackathon?**
    - `Yes, definitely`
