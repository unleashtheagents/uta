# uta — unleash the agents

A CLI agent orchestrator. Decomposes goals into parallel subtasks, dispatches them to whichever coding-agent CLI you have installed (Claude Code, Gemini CLI, more soon), and synthesizes the results — with a full recorded trajectory of every step.

Status: **pre-alpha**. v1 in active development.

## Install (once released)

```
brew install unleashtheagents/tap/uta   # macOS / Linux
go install github.com/unleashtheagents/uta/cmd/uta@latest
```

## Quick start

```
uta doctor                 # checks DB, blobs, detected providers
uta providers              # lists detected agent CLIs
uta run -g "summarize this repo in 5 bullets"
```

See `cmd/uta` for the entry point and `internal/engine` for the orchestration core.
