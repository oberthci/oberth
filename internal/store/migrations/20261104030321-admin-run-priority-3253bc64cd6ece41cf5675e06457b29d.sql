-- Purpose: Let administrators pin waiting runs to normal or low priority.
-- Rollback: Restore the pre-upgrade database backup.
CREATE TABLE run_priority_overrides_next (
 run_id TEXT PRIMARY KEY REFERENCES runs(id) ON DELETE CASCADE,
 priority TEXT NOT NULL CHECK(priority IN ('low','normal','high','highest')),
 reason TEXT NOT NULL CHECK(length(reason) BETWEEN 1 AND 512),
 actor TEXT NOT NULL,
 updated_at INTEGER NOT NULL
) WITHOUT ROWID;
INSERT INTO run_priority_overrides_next SELECT run_id,priority,reason,actor,updated_at FROM run_priority_overrides;
DROP TABLE run_priority_overrides;
ALTER TABLE run_priority_overrides_next RENAME TO run_priority_overrides;
