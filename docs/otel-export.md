# OpenTelemetry trace export

Every uta run is recorded as a trajectory. `uta trajectory --format otlp`
re-renders one as an OpenTelemetry trace (OTLP/JSON, [GenAI semantic
conventions](https://opentelemetry.io/docs/specs/semconv/gen-ai/)) so
you can inspect uta runs in any OTel-compatible backend — Jaeger,
Langfuse, Phoenix, Laminar, Grafana Tempo, or a vanilla
otel-collector.

## Usage

```sh
# To a file
uta trajectory <session-id> --format otlp > trace.json

# Straight to a collector (OTLP/HTTP)
uta trajectory <session-id> --format otlp --otlp-endpoint http://localhost:4318
```

The endpoint flag accepts a base URL (`/v1/traces` is appended) or the
full path.

## Span model

```
invoke_agent uta                 root — the whole session
├─ plan                          planner window
├─ subtask <title>               one per dispatched subtask
│    · gen_ai.agent.name         worker provider (claude/gemini/...)
│    · gen_ai.usage.input_tokens / output_tokens
│    · uta.usage.usd_cents
│    · span events: tool calls, gate outcomes, HITL, budget warnings
└─ synthesis                     synthesis window
```

Failed subtasks map to OTLP status ERROR with the error text as the
status message; running sessions stay UNSET.

## Deterministic ids

Trace and span ids are derived (SHA-256) from session / subtask ids, so
re-exporting the same session produces byte-identical output and
collectors dedup instead of duplicating.

## Quick local viewer

```sh
docker run --rm -p 16686:16686 -p 4318:4318 jaegertracing/all-in-one
uta trajectory <id> --format otlp --otlp-endpoint http://localhost:4318
open http://localhost:16686   # service "uta"
```
