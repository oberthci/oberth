package store

const publicationRetryMigrationVersion = 38
const publicationRetrySchema = `
ALTER TABLE publications ADD COLUMN attempts INTEGER NOT NULL DEFAULT 0 CHECK(attempts BETWEEN 0 AND 5);
ALTER TABLE publications ADD COLUMN retry_after INTEGER NOT NULL DEFAULT 0;
DROP TRIGGER publications_guard_update;
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
            NEW.status IN ('delivered', 'failed')
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

DROP TRIGGER promotions_guard_update;
CREATE TRIGGER promotions_guard_update
BEFORE UPDATE ON promotions
WHEN NOT (
    OLD.status = 'pending'
    AND NEW.sequence = OLD.sequence
    AND NEW.id = OLD.id
    AND NEW.repo_id = OLD.repo_id
    AND NEW.source_branch = OLD.source_branch
    AND NEW.source_sha = OLD.source_sha
    AND NEW.target_ref = OLD.target_ref
    AND NEW.actor = OLD.actor
    AND NEW.grant_id = OLD.grant_id
    AND NEW.created_at = OLD.created_at
    AND (
        (
            NEW.status = 'pending'
            AND NEW.error = OLD.error
            AND (
                (NEW.previous_sha = OLD.previous_sha AND NEW.result_sha = OLD.result_sha)
                OR (
                    OLD.previous_sha = '' AND OLD.result_sha = ''
                    AND NEW.previous_sha != '' AND NEW.result_sha != ''
                )
            )
            AND (NEW.run_id = OLD.run_id OR OLD.run_id = '')
        )
        OR (
            NEW.status IN ('passed', 'failed', 'interrupted')
            AND NEW.previous_sha = OLD.previous_sha
            AND NEW.result_sha = OLD.result_sha
            AND (NEW.run_id = OLD.run_id OR OLD.run_id = '')
        )
    )
)
AND NOT (
 OLD.status='pending' AND NEW.status='pending'
 AND NEW.sequence=OLD.sequence AND NEW.id=OLD.id AND NEW.repo_id=OLD.repo_id
 AND NEW.source_branch=OLD.source_branch AND NEW.source_sha=OLD.source_sha
 AND NEW.target_ref=OLD.target_ref AND NEW.actor=OLD.actor AND NEW.grant_id=OLD.grant_id
 AND NEW.previous_sha=OLD.previous_sha AND NEW.result_sha=OLD.result_sha
 AND NEW.run_id=OLD.run_id AND NEW.created_at=OLD.created_at
)
BEGIN
    SELECT RAISE(ABORT, 'promotion transition is immutable');
END;
`
