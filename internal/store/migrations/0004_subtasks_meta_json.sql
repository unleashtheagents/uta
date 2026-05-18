-- Give subtasks the same arbitrary-metadata escape hatch sessions already have,
-- so callers can record provider-specific data (token usage, model versions,
-- per-step latency, etc.) against an individual orchestration step.
ALTER TABLE subtasks ADD COLUMN meta_json TEXT NOT NULL DEFAULT '{}';
