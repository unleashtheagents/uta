-- Cross-mode institutional memory. A fact is a salient takeaway from one
-- run that future runs (potentially in a different mode) should consider
-- when planning. v1 retrieval is tag-overlap with the planner's goal;
-- vector search is out of scope.
--
-- tags is stored as a normalized, space-separated list of lowercase tokens
-- so SQLite LIKE searches against single tokens stay cheap on small fact
-- counts; the read path also re-tokenizes in Go to score overlap.
CREATE TABLE institutional_facts (
  id                INTEGER PRIMARY KEY AUTOINCREMENT,
  source_mode       TEXT NOT NULL DEFAULT '',
  source_session_id TEXT,
  kind              TEXT NOT NULL,
  body              TEXT NOT NULL,
  created_at        INTEGER NOT NULL,
  tags              TEXT NOT NULL DEFAULT ''
);

CREATE INDEX idx_institutional_facts_created_at ON institutional_facts(created_at DESC);
CREATE INDEX idx_institutional_facts_kind ON institutional_facts(kind);
CREATE INDEX idx_institutional_facts_source_mode ON institutional_facts(source_mode);
