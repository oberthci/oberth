package store

const publicationAbandonMigrationVersion = 41

// Rebuild only the outbox CHECK constraint; preserve intent identities and guards.
const publicationAbandonSchema = `
CREATE TABLE publications_next (
    sequence INTEGER PRIMARY KEY AUTOINCREMENT,
    id TEXT NOT NULL UNIQUE,
    repo_id INTEGER NOT NULL REFERENCES repositories(id),
    run_id TEXT REFERENCES runs(id),
    promotion_id TEXT REFERENCES promotions(id),
    ref_kind TEXT NOT NULL CHECK (ref_kind IN ('branch', 'tag')),
    ref TEXT NOT NULL,
    previous_sha TEXT NOT NULL DEFAULT '',
    previous_known INTEGER NOT NULL DEFAULT 0 CHECK (previous_known IN (0, 1)),
    result_sha TEXT NOT NULL,
    actor TEXT NOT NULL,
    status TEXT NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'delivered', 'failed', 'abandoned')),
    error TEXT NOT NULL DEFAULT '',
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL,
    attempts INTEGER NOT NULL DEFAULT 0 CHECK(attempts BETWEEN 0 AND 5),
    retry_after INTEGER NOT NULL DEFAULT 0,
    CHECK (run_id IS NOT NULL OR promotion_id IS NOT NULL)
);
INSERT INTO publications_next SELECT sequence,id,repo_id,run_id,promotion_id,ref_kind,ref,previous_sha,previous_known,result_sha,actor,status,error,created_at,updated_at,attempts,retry_after FROM publications;
DROP TRIGGER publications_guard_update;
DROP TRIGGER publications_no_delete;
DROP TABLE publications;
ALTER TABLE publications_next RENAME TO publications;
CREATE UNIQUE INDEX publications_run_idx ON publications(run_id) WHERE run_id IS NOT NULL;
CREATE UNIQUE INDEX publications_promotion_idx ON publications(promotion_id) WHERE promotion_id IS NOT NULL;
CREATE INDEX publications_pending_idx ON publications(status, sequence);
CREATE TRIGGER publications_guard_update
BEFORE UPDATE ON publications
WHEN NOT (
    OLD.status = 'pending'
    AND NEW.sequence = OLD.sequence
    AND NEW.id = OLD.id
    AND NEW.repo_id = OLD.repo_id
    AND NEW.run_id IS OLD.run_id
    AND NEW.promotion_id IS OLD.promotion_id
    AND NEW.ref_kind = OLD.ref_kind
    AND NEW.ref = OLD.ref
    AND NEW.result_sha = OLD.result_sha
    AND NEW.created_at = OLD.created_at
    AND NEW.attempts = OLD.attempts
    AND NEW.retry_after = OLD.retry_after
    AND (
        (
            NEW.status IN ('delivered', 'failed', 'abandoned')
            AND NEW.previous_sha = OLD.previous_sha
            AND NEW.previous_known = OLD.previous_known
        )
        OR (
            NEW.status = 'pending'
            AND OLD.previous_known = 0
            AND OLD.previous_sha = ''
            AND NEW.previous_known = 1
            AND NEW.error = OLD.error
        )
    )
)
AND NOT (
    OLD.status = 'failed'
    AND NEW.sequence = OLD.sequence
    AND NEW.id = OLD.id
    AND NEW.repo_id = OLD.repo_id
    AND NEW.run_id IS OLD.run_id
    AND NEW.promotion_id IS OLD.promotion_id
    AND NEW.ref_kind = OLD.ref_kind
    AND NEW.ref = OLD.ref
    AND NEW.result_sha = OLD.result_sha
    AND NEW.created_at = OLD.created_at
    AND NEW.status = 'pending'
    AND NEW.error = ''
    AND NEW.previous_sha = ''
    AND NEW.previous_known = 0
    AND NEW.attempts = 0 AND NEW.retry_after = 0
)
AND NOT (
    OLD.status = 'pending' AND NEW.status = 'pending'
    AND NEW.sequence = OLD.sequence AND NEW.id = OLD.id
    AND NEW.repo_id = OLD.repo_id AND NEW.run_id IS OLD.run_id
    AND NEW.promotion_id IS OLD.promotion_id AND NEW.ref_kind = OLD.ref_kind
    AND NEW.ref = OLD.ref AND NEW.result_sha = OLD.result_sha
    AND NEW.previous_sha = OLD.previous_sha AND NEW.previous_known = OLD.previous_known
    AND NEW.actor = OLD.actor AND NEW.created_at = OLD.created_at
    AND NEW.attempts IN (OLD.attempts, OLD.attempts + 1)
    AND NEW.attempts BETWEEN 1 AND 5
)
BEGIN
    SELECT RAISE(ABORT, 'publication transition is immutable');
END;

CREATE TRIGGER publications_no_delete BEFORE DELETE ON publications BEGIN
 SELECT RAISE(ABORT, 'publications are append-only'); END;
`
