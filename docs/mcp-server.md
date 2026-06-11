# uta as an MCP server

`uta serve --mcp` makes the entire uta CLI callable from any [Model Context
Protocol](https://spec.modelcontextprotocol.io) client. Wire it into Claude
Code, Cursor, or any other MCP-aware agent and they gain — via a single
process they launch on stdio — uta's planner/fan-out engine, idea backlog,
trajectory database, audit reflector, and inter-agent whiteboard.

Protocol version advertised: `2024-11-05`. JSON-RPC 2.0 over stdin/stdout.

## Tool catalog

| Tool | Mutates? | Use it to |
|---|---|---|
| `uta_providers_list`    | no  | inspect detected agent CLIs and their capabilities |
| `uta_ideas_list`        | no  | scan the project's improvement backlog |
| `uta_ideas_add`         | yes | push a new idea to the backlog |
| `uta_ideas_show`        | no  | fetch one idea by id |
| `uta_sessions_list`     | no  | list recent orchestration runs |
| `uta_trajectory_get`    | no  | replay the full event timeline for one session |
| `uta_audit`             | yes | run one Reflector audit pass over a directory (audit-only, no auto-fix) |
| `uta_run`               | yes | one-shot goal execution through the supervisor |
| `uta_improve_pick`      | no  | inspect the next idea the improve loop would pick |
| `uta_whiteboard_set`    | yes | post a note to the inter-agent whiteboard |
| `uta_whiteboard_get`    | no  | read the latest note for one key |
| `uta_whiteboard_list`   | no  | list the latest note per distinct key |

The mutation surface is narrow on purpose — the MCP-client model is
cooperative, not blanket trust. Notably, `uta_audit` runs **one Reflector
iteration with no auto-fix**: the calling agent receives findings; the
human (or a follow-up `uta improve` run) decides what to do.

## Project scope

If `uta serve --mcp` is launched inside a project tree (one that has a
`.uta/project.yaml`), every tool that reads or writes session data, ideas,
or whiteboard entries is **project-scoped**. Outside a project, the same
tools operate against your global `~/.uta/` home.

The stderr banner uta prints at startup tells you which scope is active:

```
uta MCP server ready · proto=2024-11-05 · tools=12 · project=myrepo (/abs/path)
```

## Wire it into Claude Code

Drop this into `.mcp.json` at the root of the repo you want to expose:

```json
{
  "mcpServers": {
    "uta": {
      "command": "uta",
      "args": ["serve", "--mcp"]
    }
  }
}
```

Claude Code will spawn `uta serve --mcp` on stdio, advertise the tool
catalog to the model, and call into uta when the model picks one. A
ready-to-copy version lives at [`examples/mcp/claude-code.mcp.json`](../examples/mcp/claude-code.mcp.json).

## Wire it into Cursor

`~/.cursor/mcp.json` (or per-project `.cursor/mcp.json`):

```json
{
  "mcpServers": {
    "uta": {
      "command": "uta",
      "args": ["serve", "--mcp"]
    }
  }
}
```

A copy lives at [`examples/mcp/cursor.mcp.json`](../examples/mcp/cursor.mcp.json).

## Generic clients

Any MCP client that supports stdio transport works. The launch command is
always:

```sh
uta serve --mcp
```

`uta serve --print-mcp-config <client>` emits a ready-to-paste config
block for `claude-code`, `cursor`, or `generic`.

## Example: Claude Code → uta orchestrate → Gemini repo-context → back to Claude Code

Once uta is wired in as an MCP server, a single user message to Claude
Code can fan a whole repo-wide question out to Gemini (which has the
million-token context window) while Claude orchestrates and synthesizes:

```
You: Use uta to ask Gemini for a repo-wide summary of all auth code,
     then review it.

Claude: [calls uta_run with goal="repo-wide summary of all auth code",
         worker="gemini"]
         → uta planner decomposes into subtasks
         → Gemini executes each with full repo context
         → uta synthesizes
         → Claude receives the final answer and the session_id
        [calls uta_trajectory_get to read the per-subtask findings]
        [returns its own review of Gemini's summary]
```

Every step lands in uta's trajectory DB; `uta sessions` / `uta dash`
shows it the same as if you'd typed `uta run` yourself.

## Safety posture

- `uta_run` and `uta_audit` execute real provider CLIs on real
  directories. The caller controls the workdir argument; the cooperative
  MCP-client model assumes the caller is acting in good faith.
- `uta_audit` is **audit-only** through MCP. To act on findings, the user
  runs `uta improve` (or `uta audit --max-iterations >1`) themselves at
  the terminal.
- HITL and capability gates remain in force: a profile that requires a
  human-signed approval to write outside the workdir still does so when
  `uta_run` is invoked from MCP.
- Budgets remain enforced. A workspace-level cap on `~/.uta/budget.yaml`
  applies to MCP-driven runs too.

## Troubleshooting

| Symptom | Likely cause |
|---|---|
| Client doesn't list `uta_*` tools | `uta` not on PATH from the client's launch env |
| `no provider on PATH` from `uta_run` | install at least one of: `claude`, `gemini`, or drop a YAML in `~/.uta/providers/` |
| Sessions list empty inside a project | the project's `.uta/` was deleted, or the client launched uta outside the project tree |
| Tool calls hang | check stderr of the spawned process — auth prompts, version mismatches, etc. surface there |

## See also

- [`internal/mcp/`](../internal/mcp/) — the protocol implementation.
- [`internal/cli/serve.go`](../internal/cli/serve.go) — tool registration.
- [`examples/mcp/`](../examples/mcp/) — copy-pasteable client configs.
- [`tests/e2e/mcp_serve_smoke_test.go`](../tests/e2e/mcp_serve_smoke_test.go) — end-to-end verification.
