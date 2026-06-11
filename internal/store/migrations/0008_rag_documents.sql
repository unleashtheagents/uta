-- RAG document store: the retrieval layer over institutional memory.
-- rag_documents is the canonical row (what was indexed, where it came
-- from); rag_fts is the FTS5/BM25 index over the body. The vector side
-- (vec0 virtual table) is created lazily by internal/memory because its
-- dimension depends on the configured embedder; rag_vec_meta records
-- which embedder/dimension the vectors were built with so a model
-- switch can trigger a rebuild instead of silently mixing spaces.
CREATE TABLE rag_documents (
    id          INTEGER PRIMARY KEY,
    kind        TEXT NOT NULL,             -- fact | final_answer | retrospective
    ref         TEXT NOT NULL DEFAULT '',  -- source row id (fact id, session id, file path)
    mode_name   TEXT NOT NULL DEFAULT '',
    body        TEXT NOT NULL,
    created_at  INTEGER NOT NULL,          -- unix nanos
    embedded    INTEGER NOT NULL DEFAULT 0 -- 1 once a vector exists in rag_vec
);

CREATE INDEX idx_rag_documents_kind ON rag_documents(kind);
CREATE INDEX idx_rag_documents_ref ON rag_documents(kind, ref);

CREATE VIRTUAL TABLE rag_fts USING fts5(
    body,
    content='rag_documents',
    content_rowid='id'
);

CREATE TABLE rag_vec_meta (
    id        INTEGER PRIMARY KEY CHECK (id = 1),
    embedder  TEXT NOT NULL,
    dim       INTEGER NOT NULL
);
