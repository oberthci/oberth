-- Purpose: Retain Git-parsed references from each run's pinned commit for display.
-- Rollback: Restore the pre-upgrade database backup.
-- Empty means not read (including pre-upgrade runs); [] means read, no trailers.
ALTER TABLE runs ADD COLUMN issue_trailers TEXT NOT NULL DEFAULT '';

CREATE TRIGGER run_issue_trailers_immutable BEFORE UPDATE OF issue_trailers ON runs
WHEN OLD.issue_trailers <> '' AND NEW.issue_trailers <> OLD.issue_trailers
BEGIN SELECT RAISE(ABORT, 'run issue trailers are immutable'); END;
