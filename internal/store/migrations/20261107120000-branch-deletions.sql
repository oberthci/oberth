-- Purpose: Record accepted branch deletions whose upstream propagation runs after the receive returns.
-- Rollback: Restore the pre-upgrade database backup.
CREATE TABLE IF NOT EXISTS branch_deletions (
 sequence INTEGER PRIMARY KEY AUTOINCREMENT,
 id TEXT NOT NULL UNIQUE,
 repo_id INTEGER NOT NULL REFERENCES repositories(id),
 branch TEXT NOT NULL,
 old_sha TEXT NOT NULL DEFAULT '',
 actor TEXT NOT NULL,
 source TEXT NOT NULL CHECK(source IN ('push','prune')),
 batch_id TEXT NOT NULL DEFAULT '',
 status TEXT NOT NULL DEFAULT 'pending' CHECK(status IN ('pending','propagated','failed','skipped')),
 attempts INTEGER NOT NULL DEFAULT 0 CHECK(attempts>=0),
 retry_after INTEGER NOT NULL DEFAULT 0,
 error TEXT NOT NULL DEFAULT '',
 created_at INTEGER NOT NULL,
 updated_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS branch_deletions_pending_idx ON branch_deletions(status, retry_after, sequence);
CREATE INDEX IF NOT EXISTS branch_deletions_repo_idx ON branch_deletions(repo_id, sequence);
