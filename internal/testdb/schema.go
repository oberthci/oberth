package testdb

// PublicationV38Schema restores the pre-abandon publication table for historical fixtures.
const PublicationV38Schema = `
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
    status TEXT NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'delivered', 'failed')),
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

CREATE TRIGGER publications_no_delete BEFORE DELETE ON publications BEGIN
 SELECT RAISE(ABORT, 'publications are append-only'); END;
DELETE FROM schema_migrations WHERE version>=43;

`

// PostV38Downgrade removes every later schema object before historical fixtures
// drop referenced columns. Restore the outbox CHECK as well as its guards.
const PostV38Downgrade = PostV45Downgrade + PublicationV38Schema + `
DROP TRIGGER IF EXISTS issues_metadata_revision;
ALTER TABLE issues DROP COLUMN revision;
ALTER TABLE token_credentials DROP COLUMN expires_at;
DROP TABLE IF EXISTS run_priority_requests;
DROP TABLE IF EXISTS priority_grants;
DROP TABLE IF EXISTS restart_workflow_timings;
DROP TABLE IF EXISTS restart_step_timings;
DROP INDEX IF EXISTS runs_latest_ref_idx;
CREATE INDEX runs_latest_ref_idx ON runs(repo_id, ref_kind, ref, queue_sequence DESC);
DELETE FROM schema_migrations WHERE version>=39;
`

// PostV45Downgrade removes later capability/retirement/ceiling objects before
// reconstructed restart and priority fixtures replay the real migrations.
const PostV45Downgrade = PostV49Downgrade + `
DROP TABLE IF EXISTS resource_ceilings;
DROP TRIGGER IF EXISTS vm_suite_executions_retired_insert;
DROP TRIGGER IF EXISTS vm_suite_executions_retired_update;
DROP TRIGGER IF EXISTS vm_suite_executions_retired_delete;
DROP TRIGGER IF EXISTS vm_suite_resources_retired_insert;
DROP TRIGGER IF EXISTS vm_suite_resources_retired_update;
DROP TRIGGER IF EXISTS vm_suite_resources_retired_delete;
DROP TABLE IF EXISTS workflow_capability_grants;
DELETE FROM schema_migrations WHERE version>=46;
`

// PostV53Downgrade reconstructs the pre-park schema while preserving trains.
const PostV53Downgrade = PostV54Downgrade + `
DROP TRIGGER IF EXISTS promotions_branch_ci_immutable;
ALTER TABLE promotions DROP COLUMN branch_ci_skipped;
DELETE FROM schema_migrations WHERE version>=54;
`

// PostV49Downgrade removes work-status and policy-wait additions while keeping
// the covering run-list index installed by the small v0.17.83 release.
const PostV49Downgrade = PostV52Downgrade + `
DROP TRIGGER IF EXISTS issues_metadata_revision;
ALTER TABLE issues DROP COLUMN work_status;
CREATE TRIGGER issues_metadata_revision
 AFTER UPDATE OF title, state, scope_upstream_id ON issues
 WHEN NEW.title IS NOT OLD.title OR NEW.state IS NOT OLD.state OR NEW.scope_upstream_id IS NOT OLD.scope_upstream_id
 BEGIN UPDATE issues SET revision=revision+1 WHERE id=NEW.id; END;
DROP TRIGGER IF EXISTS publication_policy_wait_cleanup;
DROP TABLE IF EXISTS publication_policy_waits;
DELETE FROM schema_migrations WHERE version>=50;
`

// PostIntegerMigrationsDowngrade restores the historical ledger shape in
// fixtures derived from a current database. Receipt ordinals above 55 are local
// applications of timestamp IDs, never integer migration definitions.
const PostIntegerMigrationsDowngrade = `
DROP TRIGGER IF EXISTS run_issue_trailers_immutable;
ALTER TABLE runs DROP COLUMN issue_trailers;
ALTER TABLE runs DROP COLUMN failure_cause;
DROP TABLE IF EXISTS deferred_branch_receives;
DROP TABLE IF EXISTS queue_cap;
DROP TABLE IF EXISTS concurrency_group_declarations;
ALTER TABLE runs DROP COLUMN concurrency_limit;
DROP TABLE IF EXISTS admission_holds;
DELETE FROM schema_migrations WHERE version>55;
DROP INDEX schema_migrations_id_unique;
ALTER TABLE schema_migrations DROP COLUMN id;
`

// PostV52Downgrade reconstructs the schema before durable promotion trains.
const PostV52Downgrade = PostV53Downgrade + `
DROP TRIGGER IF EXISTS promotion_train_identity_immutable;
DROP TABLE IF EXISTS promotion_train_grants;
ALTER TABLE promotions DROP COLUMN train_json;
ALTER TABLE promotions DROP COLUMN train_key;
DELETE FROM schema_migrations WHERE version>=53;
`

// PostV54Downgrade removes green-trailer receipts before reconstructing park fixtures.
const PostV54Downgrade = PostIntegerMigrationsDowngrade + `
DROP TABLE IF EXISTS issue_trailer_publications;
DELETE FROM schema_migrations WHERE version>=55;
`
