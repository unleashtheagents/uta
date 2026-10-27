-- File checkpoints: the state of a workspace file right before an agent
-- tool call changed it. before_ref points at a content-addressed blob
-- (empty when the file did not exist yet). `uta undo <session>` and the
-- shell's /rewind restore the earliest checkpoint per path; /diff shows
-- what changed since.
CREATE TABLE file_checkpoints (
    id          INTEGER PRIMARY KEY,
    session_id  TEXT NOT NULL,
    subtask_id  TEXT,
    seq         INTEGER NOT NULL,
    ts          INTEGER NOT NULL,          -- unix nanos
    tool        TEXT NOT NULL,             -- write_file | edit_file | ...
    path        TEXT NOT NULL,             -- relative to the workdir
    workdir     TEXT NOT NULL DEFAULT '',
    existed     INTEGER NOT NULL DEFAULT 1,
    before_ref  TEXT NOT NULL DEFAULT ''   -- blob path; '' when existed = 0
);

CREATE INDEX idx_file_checkpoints_session ON file_checkpoints(session_id, seq);
