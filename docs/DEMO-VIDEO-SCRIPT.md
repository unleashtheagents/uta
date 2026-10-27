# `uta-devpost-hackathon` — 2:45 Demo Video Narrator Script & Storyboard

- **Target Runtime:** `2:45` (leaves a 15-second safety buffer under the 3:00 Devpost cap)
- **Target Track:** `Coding and agentic engineering` + `$3,000 Best Use of Tavily` Bonus Award
- **Who is Watching:** Judges from **Nebius** (Token Factory team), **NVIDIA** (Nemotron team), and **Tavily** (3 judges: Evan Rimer, Ricky Johnson, Lakshya Prakash Agarwal).
- **Interactive Demo Helper:** Run `./examples/hello-nemotron/demo-recording.sh` in your terminal to step through every command cleanly on cue.

---

## How to Impress the Judges in the First 20 Seconds

1. **Lead with the 5.5× Speedup & 90/10 Economics:** Judges see dozens of single-prompt wrappers. Show immediately that `uta` is a **pure-Go multi-agent operating system** and programming language (`steer`) that routes **10% of tokens** (DAG planning & synthesis) to **NVIDIA Nemotron 3 Ultra (550B/55B MoE)** and **90% of parallel worker subtasks** to **Nemotron 3 Super (120B/12B MoE)** and **Nemotron 3 Nano (30B/3B MoE)** on **Nebius Token Factory**—running **5.5× faster (2.016s vs 11.1s)** at **134.4 tokens/sec**.
2. **Prove Real Agentic Tool Use on a Real Repo:** Show `uta trajectory` proving that **Nemotron 3 Ultra** autonomously invokes `read_file`, `write_file`, `run_command`, and **multi-hop `tavily_search`** tool calls on Nebius Token Factory.
3. **Show Both CLI + Web Dashboard + `unleashtheagents.ai`:** Keep a 60/40 split screen (Terminal on the left, `https://unleashtheagents.ai` / `uta serve --web` dashboard on the right).

---

## Scene-by-Scene Narrator Script (`2:45` Total)

### Scene 1 — The Hook & The 90/10 Nemotron Architecture (`0:00 – 0:25`)

**Visual on Screen:**
- Split screen: `https://unleashtheagents.ai` hero & `#uta` section on the right; clean dark terminal on the left.
- Run:
  ```sh
  uta nebius status
  uta nebius ping
  ```

**Narrator (Voiceover):**
> "Single coding-agent CLIs are great at one sequential conversation—but real engineering takes parallel teams, deterministic budgets, and verifiable audit trails. And if you run every parallel worker on a closed frontier model, you hit a wall on both latency and cost.
>
> Meet **uta — Unleash The Agents**: an open-source, zero-CGO Go orchestrator and **steer** programming language built for **Nebius Token Factory** and **NVIDIA Nemotron**.
>
> Instead of one monolithic model, `uta` implements a **90/10 Hybrid Mamba-Transformer MoE architecture**: routing 10 percent of tokens—architectural DAG planning and final synthesis—to **Nemotron 3 Ultra (550B)**, while fanning out 90 percent of parallel coding subtasks to **Nemotron 3 Super (120B)** and **Nemotron 3 Nano (30B)**, guarded in under 50 milliseconds by **Nemotron 3.5 Lightning**."

---

### Scene 2 — Unified `agy` / `claude` Skills + 4 Nemotron Models Running in Parallel (`0:25 – 1:05`)

**Visual on Screen:**
- In the terminal, run:
  ```sh
  uta skills list
  uta nebius route "design a zero-trust MCP gateway and add unit tests"
  uta run -f examples/hello-nemotron/uta.yaml -y
  ```
- Highlight the 4 subtasks completing in parallel in **7 seconds** (`s1_ultra_architect`, `s2_super_worker`, `s3_nano_summarizer`, `s4_lightning_sentinel`).

**Narrator (Voiceover):**
> "Inspired by Google Antigravity and Claude Code, `uta skills` automatically discovers hierarchical `AGENTS.md` and `CLAUDE.md` rules, progressive `SKILL.md` skills, and expands `@file` mentions so cloud-hosted Nemotron models have complete repository context.
>
> Watch what happens when we run our `hello-nemotron` workflow on Nebius Token Factory. Using inline `[model:ultra]`, `[model:super]`, `[model:nano]`, and `[model:lightning]` directives, `uta` dispatches four subtasks concurrently across four distinct NVIDIA Nemotron models—inspecting local Go code, executing parallel worker turns, compacting context, and verifying OWASP Agentic ASI01 through ASI10 guardrails—all synthesized in just **seven seconds**."

---

### Scene 3 — Live Runtime Tavily Grounding + Autonomous Multi-Hop Tool Calling (`1:05 – 1:50`)

**Visual on Screen:**
- Run live Tavily search + Nemotron synthesis:
  ```sh
  uta nebius search "NVIDIA Nemotron 3 hybrid Mamba-2 Transformer MoE architecture" --max-results 3
  ```
- Then show the autonomous agentic tool-calling run and its SQLite trajectory:
  ```sh
  NEBIUS_MODEL=ultra UTA_NEBIUS_AGENTIC=1 uta run -g "Call the tavily_search tool for 'NVIDIA Nemotron 3 arxiv 2512.20856' and summarize the key throughput improvement in 2 sentences" --worker nebius --planner nebius --synth nebius --pre-approve "*" --max-subtasks 1 -y
  uta trajectory 29643605
  ```

**Narrator (Voiceover):**
> "Coding agents shouldn't hallucinate documentation or benchmarks. `uta` integrates a native runtime **Tavily Search API** client—both as a direct `uta nebius search` command that synthesizes live citations into `.uta/context/RESEARCH_DATABASE.md`, and as an autonomous `tavily_search` function tool exposed directly to NVIDIA Nemotron on Nebius Token Factory.
>
> When we run `uta` in agentic mode with `read_file`, `write_file`, `edit_file`, `run_command`, and `tavily_search`, look at the recorded SQLite trajectory: **Nemotron 3 Ultra** autonomously chains **two sequential Tavily API searches**, extracts the exact 4x and 5x Mamba-2 MoE throughput gains from arXiv, and synthesizes the verified engineering report in ten seconds."

---

### Scene 4 — `steer`: Programming Language for Agents (`1:50 – 2:20`)

**Visual on Screen:**
- Show `examples/hello-nemotron/hello-nemotron.steer` and run:
  ```sh
  uta mission check examples/hello-nemotron/hello-nemotron.steer
  uta mission test examples/hello-nemotron/hello-nemotron.steer --script examples/hello-nemotron/hello-nemotron.journal.yaml
  uta mission run examples/hello-nemotron/hello-nemotron.steer --worker nebius
  ```

**Narrator (Voiceover):**
> "For mission-critical workflows, shell flags aren't enough—so `uta` ships with **steer**, a statically checked programming language for agents where inference is an effect and token budgets are linear types enforced by the compiler.
>
> In `hello-nemotron.steer`, we chain `ultra_plan`, `super_implement`, `nano_compact`, and `lightning_guardrail` under a strict 150k-token, one-dollar cap. We can verify it statically with `uta mission check`, replay it deterministically offline in CI with `uta mission test`, and run it live on Nebius Token Factory—producing a complete OWASP ASI01 through ASI10 security verdict."

---

### Scene 5 — Live Benchmark Proof (`uta bench`), Web Dashboard & Close (`2:20 – 2:45`)

**Visual on Screen:**
- Show the `uta bench` output table and `uta serve --web` dashboard (`http://127.0.0.1:8080`) alongside `https://unleashtheagents.ai`.
  ```text
  PROVIDER     STATUS  DURATION  TOKENS_IN  TOKENS_OUT  THROUGHPUT
  nebius       ok      2.016s    1264       271         134.4 tok/s
  agy          ok      10.592s   15         0           -
  claude       ok      11.099s   36391      536         48.3 tok/s
  ```

**Narrator (Voiceover):**
> "And the numbers speak for themselves. When we run `uta bench` live across our local agent CLIs and Nebius Token Factory, **NVIDIA Nemotron on Nebius completes turns in 2.01 seconds at 134.4 tokens per second—5.5 times faster in wall-clock latency and nearly 3 times higher in token throughput** than local CLI cold starts.
>
> Whether you're shipping autonomous pull requests with `uta pr`, moderating cross-model debates with `uta discuss`, or monitoring live swarms in `uta serve --web`, `uta` turns Nebius Token Factory and NVIDIA Nemotron into a complete agentic operating system. Install it today at **unleashtheagents.ai**. Thank you!"
