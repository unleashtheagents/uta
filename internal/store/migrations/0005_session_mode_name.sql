-- Record the active MissionProfile ("mode") on each session row so
-- discovery surfaces (`uta mode list`, `uta dash`) can attribute usage
-- and aggregate cost per mode without having to scan trajectory_events.
-- NULL / empty means the session ran without a profile attached.
ALTER TABLE sessions ADD COLUMN mode_name TEXT;
CREATE INDEX idx_sessions_mode_name ON sessions(mode_name);
