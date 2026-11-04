-- Purpose: Persist the optional branch queue-depth cap and ordered deferred receives.
-- Rollback: Restore the pre-upgrade database backup.
CREATE TABLE IF NOT EXISTS queue_cap (
 singleton INTEGER PRIMARY KEY CHECK(singleton=1),
 max_not_started INTEGER NOT NULL DEFAULT 0 CHECK(max_not_started>=0)
);
INSERT OR IGNORE INTO queue_cap(singleton,max_not_started) VALUES(1,0);
CREATE TABLE IF NOT EXISTS deferred_branch_receives (
 sequence INTEGER PRIMARY KEY AUTOINCREMENT,
 event_id TEXT NOT NULL UNIQUE REFERENCES receive_events(id) ON DELETE CASCADE,
 run_spec TEXT NOT NULL,
 reason TEXT NOT NULL
);
