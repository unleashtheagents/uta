-- Inter-agent whiteboard. A shared JSON state that active threads / modes
-- can write to and read from, so dev and ops can leave notes for each
-- other. Append-only history: each set produces a new row; the current
-- value for a key is the latest row by ts (id breaks ties).
--
-- value_json is stored as the raw JSON text the writer supplied; readers
-- decide whether to parse it. The store accepts any UTF-8 string and the
-- application layer enforces JSON validity on write.
CREATE TABLE whiteboard (
  id              INTEGER PRIMARY KEY AUTOINCREMENT,
  key             TEXT NOT NULL,
  value_json      TEXT NOT NULL,
  author_mode     TEXT NOT NULL DEFAULT '',
  author_session  TEXT,
  ts              INTEGER NOT NULL
);

CREATE INDEX idx_whiteboard_key_ts ON whiteboard(key, ts DESC);
CREATE INDEX idx_whiteboard_ts ON whiteboard(ts DESC);
