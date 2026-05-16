CREATE TABLE sessions (
  id                 TEXT PRIMARY KEY,
  goal               TEXT NOT NULL,
  worker             TEXT NOT NULL,
  planner            TEXT,
  status             TEXT NOT NULL,
  created_at         INTEGER NOT NULL,
  completed_at       INTEGER,
  final_answer_ref   TEXT,
  workflow_path      TEXT,
  meta_json          TEXT NOT NULL DEFAULT '{}'
);

CREATE INDEX idx_sessions_created ON sessions(created_at DESC);

CREATE TABLE subtasks (
  id                    TEXT PRIMARY KEY,
  session_id            TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
  ord                   INTEGER NOT NULL,
  title                 TEXT NOT NULL,
  prompt_ref            TEXT NOT NULL,
  worker                TEXT NOT NULL,
  provider_session_id   TEXT,
  status                TEXT NOT NULL,
  started_at            INTEGER,
  completed_at          INTEGER,
  result_text           TEXT,
  raw_output_ref        TEXT,
  error                 TEXT,
  error_kind            TEXT
);

CREATE INDEX idx_subtasks_session ON subtasks(session_id, ord);

CREATE TABLE trajectory_events (
  id            INTEGER PRIMARY KEY AUTOINCREMENT,
  session_id    TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
  subtask_id    TEXT,
  seq           INTEGER NOT NULL,
  ts            INTEGER NOT NULL,
  kind          TEXT NOT NULL,
  payload_json  TEXT NOT NULL
);

CREATE INDEX idx_traj_session_seq ON trajectory_events(session_id, seq);

CREATE TABLE providers_seen (
  name           TEXT PRIMARY KEY,
  binary_path    TEXT,
  version        TEXT,
  capabilities   TEXT NOT NULL,
  last_detected  INTEGER NOT NULL,
  notes          TEXT
);
