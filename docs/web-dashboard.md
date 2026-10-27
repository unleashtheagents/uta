# uta serve --web — Local Interactive Web Dashboard

`uta serve --web` launches an interactive local HTTP server providing a real-time web dashboard for project orchestration, active sessions, ideas backlog, and provider status.

## Usage

```sh
# Launch web dashboard on default port (8080)
uta serve --web

# Launch web dashboard on custom port
uta serve --web --port 9090
```

Open your browser at `http://127.0.0.1:8080`.

## Endpoints

- `GET /`: Interactive Single Page Application (SPA) dashboard UI.
- `GET /api/dash`: Dashboard snapshot JSON (threads, sessions, alerts, ideas, retrospectives, per-mode panes).
- `GET /api/sessions`: List of recent orchestration sessions from SQLite store.
- `GET /api/providers`: List of detected agent providers and their capabilities.

## Features

- **Real-time Status Sync**: Automatically polls and updates session statuses and backlogs every 3 seconds.
- **Project-Scoped**: When launched inside a project directory, automatically displays project-scoped sessions and backlogs.
- **Dark Theme UI**: Clean, high-legibility interface styled for performance and clarity.
