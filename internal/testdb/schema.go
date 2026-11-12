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
const PostIntegerMigrationsDowngrade = PostExecutionTargetsDowngrade + `
DROP TABLE IF EXISTS resource_attempts; DROP TABLE IF EXISTS resource_attempt_inventories; DROP TABLE IF EXISTS pod_resource_dispositions;
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

// PostExecutionTargetsDowngrade removes disposable target-custody fixtures
// before reconstructing historical schemas. Production migration stays strict.
const PostExecutionTargetsDowngrade = PostPinnedSourcesDowngrade + `
DROP TABLE IF EXISTS execution_target_resources;
DROP TABLE IF EXISTS execution_target_bindings;
DELETE FROM schema_migrations WHERE id='20261105050000-execution-target-custody';
`

// PostPinnedSourcesDowngrade removes only the new source receipt from disposable
// current-schema fixtures before reconstructing an older schema and ledger.
const PostPinnedSourcesDowngrade = PostProtectedRuntimeDowngrade + `
DROP TRIGGER IF EXISTS run_source_bindings_immutable;
ALTER TABLE runs DROP COLUMN source_bindings;
DELETE FROM schema_migrations WHERE id='20261105180000-pinned-source-bindings';
`

// PostProtectedRuntimeDowngrade removes generic runtime custody in disposable
// historical fixtures before replaying older migrations. Production admissions
// remain append-only.
const PostProtectedRuntimeDowngrade = PostProtectedPlacementDowngrade + `
DROP TABLE IF EXISTS protected_runtime_admissions;
DROP INDEX IF EXISTS protected_runtime_audit_idx;
DELETE FROM schema_migrations WHERE id='20261106021113-protected-runtime-admissions-6bb6bdbe716e4f2623808c4dc612d943';
`

// PostProtectedPlacementDowngrade removes placement custody only in disposable
// historical migration fixtures. Production custody remains append-only.
const PostProtectedPlacementDowngrade = PostProtectedWorkflowDowngrade + `
DROP TABLE IF EXISTS protected_placement_admissions;
DROP INDEX IF EXISTS protected_placement_audit_idx;
DROP INDEX IF EXISTS protected_placement_source_audit_idx;
DELETE FROM schema_migrations WHERE id='20261106033000-protected-placement-admissions-96f3e81aca97478ea8d7499e6e378631';
`

// PostProtectedWorkflowDowngrade removes later parent-custody indexes from disposable historical fixtures.
const PostProtectedWorkflowDowngrade = PostBranchDeletionsDowngrade + `
DROP INDEX IF EXISTS protected_placement_workflow_audit_idx;
DROP INDEX IF EXISTS protected_placement_workflow_uid_audit_idx;
DELETE FROM schema_migrations WHERE id='20261106053000-protected-workflow-custody-f83c6b27a4904dc8bd29e1952e86430a';
`

// PostBranchDeletionsDowngrade removes the asynchronous branch deletion
// records from disposable historical fixtures.
const PostBranchDeletionsDowngrade = PostRuntimeOverridesDowngrade + `
DROP TABLE IF EXISTS branch_deletions;
DELETE FROM schema_migrations WHERE id='20261107120000-branch-deletions';
`

// PostRuntimeOverridesDowngrade removes the runtime release WIF and admission
// capacity overrides (#1497) from disposable historical fixtures.
const PostRuntimeOverridesDowngrade = PostAssistedRunsDowngrade + `
DROP TABLE IF EXISTS release_wif_overrides;
DROP TABLE IF EXISTS admission_capacity_override;
DELETE FROM schema_migrations WHERE id='20261108090000-runtime-admin-overrides';
`

// PostAssistedRunsDowngrade removes the agent-assisted continuation columns
// from disposable historical fixtures.
const PostAssistedRunsDowngrade = PostAgentEventsDowngrade + `
DROP TRIGGER IF EXISTS run_assist_requested_immutable;
DROP TRIGGER IF EXISTS run_assisted_monotonic;
ALTER TABLE runs DROP COLUMN assist_requested;
ALTER TABLE runs DROP COLUMN assisted;
ALTER TABLE runs DROP COLUMN assist_record;
DELETE FROM schema_migrations WHERE id='20261108232919-assisted-runs-2c8c05259f2ff5d590fe8162f2e1d195';
`

// PostAgentEventsDowngrade removes the MCP agent loop tables (#1514) from
// disposable historical fixtures.
const PostAgentEventsDowngrade = PostPromotionTagsDowngrade + `
DROP TABLE IF EXISTS agent_events;
DROP TABLE IF EXISTS sync_intervals;
DROP TABLE IF EXISTS promotion_prunes;
DELETE FROM schema_migrations WHERE id='20261109220004-agent-events-5a27b69187397087ddb6d1f52bd68403';
`

// PostPromotionTagsDowngrade removes promote tag_after requests (#1524) from
// disposable historical fixtures.
const PostPromotionTagsDowngrade = PostClientCertificatesDowngrade + `
DROP TABLE IF EXISTS promotion_tags;
DELETE FROM schema_migrations WHERE id='20261111132749-promotion-tags-c5ec022389eceac5d4f1120521ed20ef';
`

// PostClientCertificatesDowngrade reconstructs pre-certificate fixtures.
const PostClientCertificatesDowngrade = PostCanonicalPrioritiesDowngrade + `
DROP TABLE IF EXISTS uplink_client_pins;
ALTER TABLE token_credentials DROP COLUMN certificate_only;
DELETE FROM schema_migrations WHERE id='20261111190000-client-certificates';
`

// PostPromotionConfirmationDowngrade reconstructs historical fixtures without
// claiming a forge observation that the old server never made.
const PostPromotionConfirmationDowngrade = PostAPIReadIndexesDowngrade + `
DROP TRIGGER IF EXISTS promotions_confirmation_guard;
ALTER TABLE promotions DROP COLUMN observed_target_sha;
DELETE FROM schema_migrations WHERE id='20261112094524-promotion-forge-confirmation-edaf1e2c27e6e0e3d0d942490834bbb2';
`

// PostCanonicalPrioritiesDowngrade removes the receipt in disposable fixtures.
const PostCanonicalPrioritiesDowngrade = PostPromotionConfirmationDowngrade + `
DROP TABLE IF EXISTS node_boot_observations;
DELETE FROM schema_migrations WHERE id='20261111220000-node-boot-observations';
` + PostUplinkInvitesDowngrade + `
DROP INDEX IF EXISTS runs_ref_prefix_idx;
DELETE FROM schema_migrations WHERE id='20261111201000-run-ref-prefix-index';
DELETE FROM schema_migrations WHERE id='20261111200000-canonical-priorities';
`

// PostUplinkInvitesDowngrade removes enrollment state from disposable migration fixtures.
const PostUplinkInvitesDowngrade = `
DROP TABLE IF EXISTS uplink_invites;
DELETE FROM schema_migrations WHERE id='20261111202000-uplink-invites';
`

// PostAPIReadIndexesDowngrade removes indexes and their receipt only from
// disposable fixtures before replaying historical schemas.
const PostAPIReadIndexesDowngrade = PostNativeSchedulingDowngrade + `
DROP INDEX IF EXISTS runs_repo_active_idx;
DROP INDEX IF EXISTS runs_repo_started_idx;
DELETE FROM schema_migrations WHERE id='20261112105147-run-fair-share-indexes-d4e558943b179505491a455daf163db8';
DROP INDEX IF EXISTS runs_counts_idx;
DROP INDEX IF EXISTS runs_active_ref_idx;
DROP INDEX IF EXISTS runs_terminal_end_idx;
DROP INDEX IF EXISTS audit_actions_type_idx;
DELETE FROM schema_migrations WHERE id='20261112095139-api-read-indexes-5088d2686292709cb56a6d94bddb707b';
`

// PostNativeSchedulingDowngrade removes new removal receipts in disposable historical fixtures.
const PostNativeSchedulingDowngrade = `
DELETE FROM schema_migrations WHERE id='20261112182311-remove-run-fair-share-63db0cad7aad5e31cfcb60e26f63f922';
`
