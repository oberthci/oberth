-- Purpose: Persist server-owned runtime overrides for release WIF repository grants and admission capacity (#1497).
-- Rollback: Restore the pre-upgrade database backup.
CREATE TABLE IF NOT EXISTS release_wif_overrides (
 repo TEXT PRIMARY KEY CHECK(length(repo) BETWEEN 5 AND 512),
 revoked INTEGER NOT NULL CHECK(revoked IN (0,1)),
 grant_json TEXT NOT NULL CHECK(json_valid(grant_json) AND length(grant_json) <= 16384),
 reason TEXT NOT NULL CHECK(length(reason) BETWEEN 1 AND 512),
 actor TEXT NOT NULL CHECK(length(actor) > 0),
 updated_at INTEGER NOT NULL,
 CHECK(revoked = 0 OR grant_json = '{}')
) WITHOUT ROWID;
CREATE TABLE IF NOT EXISTS admission_capacity_override (
 singleton INTEGER PRIMARY KEY CHECK(singleton = 1),
 max_concurrent_jobs INTEGER NOT NULL CHECK(max_concurrent_jobs BETWEEN 1 AND 64),
 reason TEXT NOT NULL CHECK(length(reason) BETWEEN 1 AND 512),
 actor TEXT NOT NULL CHECK(length(actor) > 0),
 updated_at INTEGER NOT NULL
);
