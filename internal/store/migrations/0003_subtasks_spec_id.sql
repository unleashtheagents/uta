-- Add the planner-assigned spec id (e.g. "s1") to subtask rows so a row can
-- be cross-referenced against trajectory events and the original Plan.
ALTER TABLE subtasks ADD COLUMN spec_id TEXT;

CREATE INDEX idx_subtasks_session_spec ON subtasks(session_id, spec_id);
