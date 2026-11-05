-- Purpose: Retain immutable exact cross-repository source selections per run.
-- Rollback: Restore a compatible verified pre-upgrade database backup.
-- Empty is unknown historical metadata, never evidence of an empty declaration.
ALTER TABLE runs ADD COLUMN source_bindings TEXT NOT NULL DEFAULT '';
CREATE TRIGGER run_source_bindings_immutable BEFORE UPDATE OF source_bindings ON runs
WHEN OLD.source_bindings <> '' AND NEW.source_bindings <> OLD.source_bindings
BEGIN SELECT RAISE(ABORT, 'run source bindings are immutable'); END;
