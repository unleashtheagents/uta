# uta — Hackathon Demo Video (2:31, as cut)

The final video is `uta-demo.mp4` (1920×1080, 30 fps, 2:31, silent with
caption bars). It was captured with VHS against released binaries in a
fresh `/tmp` workspace with an isolated `UTA_HOME`; every number on screen
is from a live Nebius Token Factory run. This file is the voiceover script
(~330 words, fits the cut at a calm pace) and the shot log.

Rules checked: ≤ 3 minutes, shows the project working on Token Factory,
no third-party trademarks beyond nominative references, no music.

## Timecodes (final cut)

| t | scene |
|---|---|
| 0:00–0:04 | Title card |
| 0:04–0:11 | S1 `uta nebius status` — five tiers, routing rule |
| 0:11–0:27 | S2 first write asks; `[a]` saves the rule |
| 0:27–0:55 | S3 direct mode codes + tests; `/cost` 70% saved |
| 0:55–1:12 | S4 policy denies `rm -rf`; Lightning blocks piped install (208 ms) |
| 1:12–1:26 | S5 Tavily search + extract, cited bullets |
| 1:26–1:36 | S6 Super-VL reads a screenshot; `.mcp.json` tool call |
| 1:36–2:25 | S7 `uta undo` reverts the run; 3-way fan-out; cost report 66.9% saved |
| 2:25–2:31 | End card |

## Voiceover

> **[0:00 title]** This is uta — one Go binary that turns the five NVIDIA
> Nemotron tiers on Nebius Token Factory into a coding-agent team.
>
> **[0:04 S1]** One API key. The engine routes by role: Ultra plans,
> Super works, Nano compacts, Lightning guards, Super-VL sees.
>
> **[0:11 S2]** Agents don't get the keys to your machine. The first
> write asks. Answer "always", and the rule lands in a permissions file
> you can read and commit.
>
> **[0:27 S3]** Direct mode feels like any coding agent — except it's
> Nemotron Super writing a Go package, testing it, and passing — and
> slash-cost shows the run cost one cent: seventy percent less than
> running everything on the big model.
>
> **[0:55 S4]** Safety is layered. Your policy denies "rm -rf" for free,
> in zero milliseconds. What policy doesn't catch, Nemotron Lightning
> judges — here it blocks a piped install in 208 milliseconds, before
> the command ever runs.
>
> **[1:12 S5]** Research is grounded: Tavily search and extract are
> native tools, so Nemotron reads real sources and cites them.
>
> **[1:26 S6]** It sees — Super-VL reads a screenshot. And any MCP
> server in your repo becomes a native Nemotron tool.
>
> **[1:36 S7]** Every file an agent writes is checkpointed: uta undo
> shows the diff and reverts the whole run. Then the full orchestra —
> Ultra plans three subtasks, Super runs them in parallel, and the cost
> report proves it: sixty-seven percent saved, every call audited.
>
> **[2:25 end]** uta is open source, MIT licensed. brew install, run
> `uta nebius demo`, and every claim in this video is one command away.

## Re-recording

Scenes were recorded one VHS tape per scene (waits are unreliable after
the terminal scrolls in vhs 0.12.1 — use blocking commands, type-ahead
with uta's echo suppression, and a generous tail `Sleep`), assembled with
freeze-compression (ffmpeg freezedetect), PIL-rendered caption bars and
title/end cards. Pipeline: scratchpad `video/assemble.py`.
