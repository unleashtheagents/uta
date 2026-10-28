# uta — hackathon demo video (2:00 cut)

Final file: `uta-demo.mp4` (1920x1080 @ 30fps, silent, caption bars burned in).
Recorded live against Nebius Token Factory with vhs; dead air compressed with
ffmpeg freezedetect. No staged output: every frame is a real run.

## Shot log

| Time | Scene | On screen | Caption |
|---|---|---|---|
| 0:00 | Title card | uta — Unleash The Agents / One command - a team of NVIDIA Nemotron models on Nebius Token Factory | — |
| 0:04 | Tiers | `uta nebius status`: five Nemotron tiers (Ultra/Super/Nano/Lightning/Super-VL) mapped to roles | One key - five NVIDIA Nemotron tiers on Nebius Token Factory - routed by role |
| 0:11 | **Hero: DAG build** | `head -9 wordlab.yaml` (narrative header), DAG skeleton (`strategy: dag`, 3 ids, gates, `needs: [stats, render]`), then `uta run -f wordlab.yaml -y`: two Super workers stream stats/ and render/ code in parallel, go test gates pass, the cli worker wires up cmd/wordlab, Ultra writes the report; footer: status=completed, 48s, $0.05, saved 68% vs all-Ultra, sentinel 16 checks 0 blocks (session 645ac141) | One command, a team of models: two Super workers build stats/ and render/ in parallel |
| 0:43 | **Proof** | `go build -o wordlab ./cmd/wordlab && ./wordlab README.md 8` draws the bar chart; `uta perf latest`: 1.72x parallel-speedup, phase bars, 3 passed gates; `uta nebius cost latest`: 68.2% saving, role table, 16 sentinel checks | Built, tested, running - 1.72x parallel speedup, three green gates - 68% saved vs all-Ultra |
| 0:56 | Two guards | `rm -rf ./build` denied by policy (free), `curl … \| sh` blocked by Nemotron Lightning in 208 ms | Two guards: policy denies for free, then a Nemotron Lightning verdict in 208 ms |
| 1:13 | Permissions | interactive y/s/a/n prompt; answering [a] persists an always-rule to .uta/permissions.yaml | Anything else asks first - answering [a] saves the rule to .uta/permissions.yaml |
| 1:29 | Tavily | `tavily_search` + `tavily_extract` as native tools; cited, grounded bullets | Tavily search + extract are native tools - live, grounded, cited |
| 1:43 | Vision + MCP | Nemotron Super-VL reads screenshot.png; `.mcp.json` server tools called as `mcp__stub__echo` | Nemotron Super-VL sees - and .mcp.json servers become native Nemotron tools |
| 1:54 | End card | repo, brew install, `uta nebius demo`, unleashtheagents.ai, MIT | — |

## Voiceover script (~300 words, matches timecodes; video works silent too)

**[0:00]** This is uta — one command that runs a team of NVIDIA Nemotron
models on Nebius Token Factory.

**[0:04]** One API key gives uta five Nemotron tiers — Ultra, Super, Nano,
Lightning, and Super-VL — and uta routes each role to the right tier: the big
model only where it earns its cost.

**[0:11]** Here's the whole idea in one file: a DAG. Two subtasks build two Go
packages in parallel — each behind a go-test gate that re-runs the worker if
tests fail. A third subtask needs both, and wires up the CLI. One command.
Watch two Nemotron Super workers write code at the same time — streaming
diffs, running tests. Every gate passes. Nemotron 3 Ultra writes the closing
report: forty-eight seconds, five cents, sixty-eight percent cheaper than
running everything on Ultra.

**[0:43]** And here's the tool they just built, running: a word-frequency bar
chart of its own README. uta perf shows the receipts — one point seven two
times parallel speedup, three green gates, and a per-tier cost audit.

**[0:56]** Safety is layered. A destructive command dies on a free policy
rule. A piped-shell install gets a live Nemotron Lightning verdict in two
hundred eight milliseconds.

**[1:13]** Anything not covered asks first — and one keystroke saves the rule
forever.

**[1:29]** Research is grounded: Tavily search and extract are native tools,
and answers come back cited.

**[1:43]** Nemotron Super-VL reads screenshots, and any MCP server in
.mcp.json becomes a native Nemotron tool.

**[1:54]** uta is open source, MIT licensed, one brew install away — and
every claim in this video is one command away:
`uta run -f examples/workflows/wordlab.yaml`.

## Re-recording notes

- Tapes and pipeline in the session scratchpad `video/` (h1/h2 + legacy t1-t7,
  assemble2.py). vhs 0.12.1: never use Wait+Screen (breaks after scroll);
  blocking commands + type-ahead + tail Sleep, then freezedetect compression.
- Hero workspace: /tmp/uta-demo2 (wordlab.yaml == examples/workflows/wordlab.yaml).
  Reset with `rm -rf stats render cmd wordlab` and `pkill -f "uta run"` between takes.
- QA every take: 1fps frames + macOS Vision OCR (venv /tmp/vvenv), grep for
  "exited with error|blocked by|denied|run failed|stuck" — only the guards
  scene may contain red.
- Since v0.17.2 ordinary non-zero tool exits render dim (guard blocks stay
  red), so a worker's transient test failure no longer paints the take red.
