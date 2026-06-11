# MCP client configs for `uta serve --mcp`

Drop the relevant file at the path your client expects:

| Client | Path | File |
|---|---|---|
| Claude Code | `.mcp.json` at repo root | [`claude-code.mcp.json`](./claude-code.mcp.json) |
| Cursor (global) | `~/.cursor/mcp.json` | [`cursor.mcp.json`](./cursor.mcp.json) |
| Cursor (per repo) | `.cursor/mcp.json` at repo root | [`cursor.mcp.json`](./cursor.mcp.json) |
| Generic stdio MCP | wherever your client takes it | [`generic.mcp.json`](./generic.mcp.json) |

You can also generate any of these with:

```sh
uta serve --print-mcp-config claude-code   # or cursor / generic
```

For the full guide see [`docs/mcp-server.md`](../../docs/mcp-server.md).
