CREATE TABLE ideas (
  id            TEXT PRIMARY KEY,
  title         TEXT NOT NULL,
  body          TEXT NOT NULL,
  source        TEXT NOT NULL,         -- 'manual' | 'gather:<worker>' | 'derived:<idea-id>'
  severity      TEXT NOT NULL,         -- 'high' | 'medium' | 'low' | 'info'
  status        TEXT NOT NULL,         -- 'proposed' | 'accepted' | 'in_progress' | 'done' | 'failed' | 'rejected'
  created_at    INTEGER NOT NULL,
  updated_at    INTEGER NOT NULL,
  attempts      INTEGER NOT NULL DEFAULT 0,
  last_session  TEXT,                  -- last uta session that worked on it
  last_error    TEXT,
  summary       TEXT,                  -- short summary written on completion (diff stats, files touched)
  tags_json     TEXT NOT NULL DEFAULT '[]',
  meta_json     TEXT NOT NULL DEFAULT '{}'
);

CREATE INDEX idx_ideas_status_severity ON ideas(status, severity DESC, created_at ASC);
CREATE INDEX idx_ideas_created ON ideas(created_at DESC);
