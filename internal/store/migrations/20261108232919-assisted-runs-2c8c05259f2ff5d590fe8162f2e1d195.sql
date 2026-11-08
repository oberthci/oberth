-- Purpose: Agent-assisted continuation: the run's opt-in, the assisted verdict that excludes it from green evidence, and the append-only hold record.
-- Rollback: Restore the pre-upgrade database backup.
-- assist_requested: the push option or rerun flag that asked for retry holds.
-- assisted: set once a held attempt was continued; such a run is never
-- promotion, release or green-for-SHA evidence and is never published.
-- assist_record: JSON of every hold action (exec, patch with sha256, continue, abandon).
ALTER TABLE runs ADD COLUMN assist_requested INTEGER NOT NULL DEFAULT 0;
ALTER TABLE runs ADD COLUMN assisted INTEGER NOT NULL DEFAULT 0;
ALTER TABLE runs ADD COLUMN assist_record TEXT NOT NULL DEFAULT '';

CREATE TRIGGER run_assist_requested_immutable BEFORE UPDATE OF assist_requested ON runs
WHEN NEW.assist_requested <> OLD.assist_requested
BEGIN SELECT RAISE(ABORT, 'run assist request is immutable'); END;

CREATE TRIGGER run_assisted_monotonic BEFORE UPDATE OF assisted ON runs
WHEN OLD.assisted = 1 AND NEW.assisted <> 1
BEGIN SELECT RAISE(ABORT, 'an assisted run stays assisted'); END;
