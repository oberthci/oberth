-- Purpose: remove admission episodes while preserving durable Workflow/step timing.
-- Rollback: Restore the pre-upgrade database backup.
DROP TRIGGER IF EXISTS run_timing_grant;
DROP TRIGGER IF EXISTS run_timing_park;
DROP TRIGGER run_timing_terminal;
DROP TABLE IF EXISTS run_admission_timings;
CREATE TRIGGER run_timing_terminal AFTER UPDATE OF status ON runs
WHEN OLD.finished_at IS NULL AND NEW.finished_at IS NOT NULL
BEGIN
    INSERT OR IGNORE INTO run_timings(run_id,version) VALUES(NEW.id,1);
    UPDATE run_timings SET terminal_at=COALESCE(terminal_at,NEW.finished_at) WHERE run_id=NEW.id;
END;
