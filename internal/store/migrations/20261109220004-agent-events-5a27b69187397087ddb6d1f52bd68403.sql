-- Purpose: MCP agent loop (#1514): durable agent events, native Argo lock history and promotion prune_source requests.
-- Rollback: Restore the pre-upgrade database backup.
-- agent_events: at-least-once event log for events_since and the MCP push
-- stream; dedupe_key records one fact once; the server prunes the oldest rows
-- beyond events.retention.
CREATE TABLE IF NOT EXISTS agent_events (
 id INTEGER PRIMARY KEY AUTOINCREMENT,
 dedupe_key TEXT NOT NULL UNIQUE,
 kind TEXT NOT NULL CHECK(kind IN ('run.finished','run.held','run.lock_wait','promotion.finished','release.published','publication.failed')),
 repo_id INTEGER NOT NULL DEFAULT 0,
 repo TEXT NOT NULL DEFAULT '',
 run_id TEXT NOT NULL DEFAULT '',
 promotion_id TEXT NOT NULL DEFAULT '',
 ref TEXT NOT NULL DEFAULT '',
 sha TEXT NOT NULL DEFAULT '',
 summary TEXT NOT NULL DEFAULT '',
 data TEXT NOT NULL DEFAULT '',
 created_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS agent_events_repo_idx ON agent_events(repo_id, id);
CREATE INDEX IF NOT EXISTS agent_events_run_idx ON agent_events(run_id, id);

-- sync_intervals: sampled holder and waiter intervals of native Argo
-- semaphores and mutexes; ended_at is NULL while a participant is observed.
CREATE TABLE IF NOT EXISTS sync_intervals (
 id INTEGER PRIMARY KEY AUTOINCREMENT,
 lock_key TEXT NOT NULL,
 role TEXT NOT NULL CHECK(role IN ('holder','waiter')),
 run_id TEXT NOT NULL,
 repo_id INTEGER NOT NULL DEFAULT 0,
 repo TEXT NOT NULL DEFAULT '',
 started_at INTEGER NOT NULL,
 last_seen INTEGER NOT NULL,
 ended_at INTEGER
);
CREATE INDEX IF NOT EXISTS sync_intervals_open_idx ON sync_intervals(ended_at, run_id);
CREATE INDEX IF NOT EXISTS sync_intervals_lock_idx ON sync_intervals(lock_key, id);

-- promotion_prunes: a promote with prune_source; the source branch is pruned
-- only after the promotion passed and the upstream target equals its result.
CREATE TABLE IF NOT EXISTS promotion_prunes (
 promotion_id TEXT PRIMARY KEY REFERENCES promotions(id),
 actor TEXT NOT NULL,
 status TEXT NOT NULL DEFAULT 'pending' CHECK(status IN ('pending','pruned','kept')),
 reason TEXT NOT NULL DEFAULT '',
 batch_id TEXT NOT NULL DEFAULT '',
 requested_at INTEGER NOT NULL,
 updated_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS promotion_prunes_pending_idx ON promotion_prunes(status, requested_at);
