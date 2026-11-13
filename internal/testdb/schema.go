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
const PostNativeSchedulingDowngrade = PostNativeControlsDowngrade + `
DELETE FROM schema_migrations WHERE id='20261112182311-remove-run-fair-share-63db0cad7aad5e31cfcb60e26f63f922';
`

// PostNativeControlsDowngrade recreates removed historical fixture tables, never production compatibility policy.
const PostNativeControlsDowngrade = PostNativeHoldDowngrade + `
CREATE TABLE IF NOT EXISTS size_grants (
 repo_id INTEGER PRIMARY KEY REFERENCES repositories(id),max_size TEXT NOT NULL,
 granted_by TEXT NOT NULL,granted_at INTEGER NOT NULL,revoked_by TEXT NOT NULL DEFAULT '',revoked_at INTEGER
);
CREATE TABLE IF NOT EXISTS resource_ceilings (
 repo_id INTEGER PRIMARY KEY REFERENCES repositories(id),resources_json TEXT NOT NULL,
 granted_by TEXT NOT NULL,granted_at INTEGER NOT NULL,revoked_by TEXT NOT NULL DEFAULT '',revoked_at INTEGER
);
CREATE TABLE IF NOT EXISTS queue_cap (singleton INTEGER PRIMARY KEY,max_not_started INTEGER NOT NULL DEFAULT 0);
INSERT OR IGNORE INTO queue_cap(singleton,max_not_started) VALUES(1,0);
CREATE TABLE IF NOT EXISTS deferred_branch_receives (
 sequence INTEGER PRIMARY KEY AUTOINCREMENT,event_id TEXT NOT NULL UNIQUE REFERENCES receive_events(id),run_spec TEXT NOT NULL,reason TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS admission_capacity_override (
 singleton INTEGER PRIMARY KEY,max_concurrent_jobs INTEGER NOT NULL,reason TEXT NOT NULL,actor TEXT NOT NULL,updated_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS concurrency_group_declarations (
 repo_id INTEGER NOT NULL REFERENCES repositories(id),release INTEGER NOT NULL,group_name TEXT NOT NULL,group_limit INTEGER NOT NULL,PRIMARY KEY(repo_id,release)
);
CREATE INDEX IF NOT EXISTS concurrency_group_declarations_name ON concurrency_group_declarations(group_name);
DELETE FROM schema_migrations WHERE id='20261112182825-remove-admission-controls-1819885d447894c83b86b9f6aabfb9b1';
`

// PostNativeHoldDowngrade restores the empty hold table in disposable historical fixtures.
const PostNativeHoldDowngrade = PostNativePriorityDowngrade + `
CREATE TABLE IF NOT EXISTS admission_holds (
 singleton INTEGER PRIMARY KEY,on_hold INTEGER NOT NULL DEFAULT 0,reason TEXT NOT NULL DEFAULT '',actor TEXT NOT NULL DEFAULT '',
 allow_tag INTEGER NOT NULL DEFAULT 1,allow_release INTEGER NOT NULL DEFAULT 1,allow_promotion INTEGER NOT NULL DEFAULT 0,
 expires_at INTEGER NOT NULL DEFAULT 0,changed_at INTEGER NOT NULL DEFAULT 0
);
INSERT OR IGNORE INTO admission_holds(singleton) VALUES(1);
DELETE FROM schema_migrations WHERE id='20261112191324-remove-admin-hold-81c738135e3f7c3e6e8eadcd1eb51ea4';
`

// PostNativePriorityDowngrade recreates empty priority tables only for old-schema fixtures.
const PostNativePriorityDowngrade = PostNativeStartupDowngrade + `
CREATE TABLE IF NOT EXISTS queue_priority_patterns (pattern TEXT PRIMARY KEY) WITHOUT ROWID;
CREATE TABLE IF NOT EXISTS run_priority_overrides (run_id TEXT PRIMARY KEY REFERENCES runs(id) ON DELETE CASCADE,priority TEXT NOT NULL,reason TEXT NOT NULL,actor TEXT NOT NULL,updated_at INTEGER NOT NULL) WITHOUT ROWID;
CREATE TABLE IF NOT EXISTS run_priority_requests (run_id TEXT PRIMARY KEY REFERENCES runs(id) ON DELETE CASCADE,requested TEXT NOT NULL,accepted TEXT NOT NULL,reason TEXT NOT NULL,actor TEXT NOT NULL,refusal TEXT NOT NULL) WITHOUT ROWID;
CREATE TABLE IF NOT EXISTS priority_grants (identity TEXT PRIMARY KEY,max_class TEXT NOT NULL,expires INTEGER NOT NULL,granted_by TEXT NOT NULL) WITHOUT ROWID;
DELETE FROM schema_migrations WHERE id='20261112192937-remove-queue-priority-104b8d495586e655eeee52d3b76aaa30';
`

// PostNativeStartupDowngrade removes only the startup-cleanup migration receipt in historical fixtures.
const PostNativeStartupDowngrade = PostNativeHeadWaitDowngrade + `
DELETE FROM schema_migrations WHERE id='20261112194221-retire-startup-cleanup-reason-3cbdb8308f00d05de0af9950db4bcae9';
`

// PostNativeHeadWaitDowngrade recreates retired state only in historical fixtures.
const PostNativeHeadWaitDowngrade = PostNativeTargetPlacementDowngrade + `
CREATE TABLE IF NOT EXISTS admission_head_waits (repo_id INTEGER NOT NULL REFERENCES repositories(id) ON DELETE CASCADE,ref_kind TEXT NOT NULL CHECK(ref_kind IN ('branch','tag')),ref TEXT NOT NULL,sha TEXT NOT NULL,since INTEGER NOT NULL,PRIMARY KEY(repo_id,ref_kind,ref,sha));
DELETE FROM schema_migrations WHERE id='20261112200858-remove-head-wait-state-5f391577728baf3aa3d319f69af46253';
`

// PostNativeTargetPlacementDowngrade reconstructs only disposable old-schema fixtures.
const PostNativeTargetPlacementDowngrade = PostExtendedResourceGrantDowngrade + `
DROP TRIGGER IF EXISTS execution_target_custody_digest_immutable;
DROP TRIGGER IF EXISTS execution_target_quarantine_immutable;
ALTER TABLE execution_target_bindings DROP COLUMN quarantine_reason;
ALTER TABLE execution_target_bindings DROP COLUMN custody_digest;
DELETE FROM schema_migrations WHERE id='20261113004204-remove-execution-target-placement-9b77efae7729ff92178a88a4344e38e4';
`

// PostExtendedResourceGrantDowngrade removes only the removal receipt in disposable fixtures.
const PostExtendedResourceGrantDowngrade = PostStepMeasurementDowngrade + `
DROP TRIGGER IF EXISTS workflow_capability_recovery_run_changed;
DROP TABLE IF EXISTS workflow_capability_recoveries;
DELETE FROM schema_migrations WHERE id='20261113010858-remove-extended-resource-grants-1523724f34af80938b4963079df78930';
`

// PostWorkflowCapabilityUIDDowngrade reconstructs an empty UID-less table only in disposable historical fixtures.
// postRunGrantClassificationDowngrade reconstructs the old column only in
// disposable historical fixtures before replaying forward migrations.
const postRunGrantClassificationDowngrade = `
ALTER TABLE runs ADD COLUMN credentialed INTEGER NOT NULL DEFAULT 0 CHECK (credentialed IN (0, 1));
DELETE FROM schema_migrations WHERE id='20261113193452-remove-run-credentialed-14b11551632068123f2deef9e27ca7dc';
DELETE FROM schema_migrations WHERE id='20261113193519-scope-secret-approvals-to-workflow-4e48838f64a9b32ddba9d914280daa25';
`

const PostWorkflowCapabilityUIDDowngrade = PostExecutionTargetRemovalDowngrade + historicalRestartTimingSchema + postRunGrantClassificationDowngrade + `
DELETE FROM schema_migrations WHERE id='20261113181825-remove-restart-timings-370d05c58486149250a7139eb6afdca7';
DELETE FROM schema_migrations WHERE id='20261113153226-stop-restart-resubmission-c9820cdaf74239b4216c84bafb350153';
DELETE FROM schema_migrations WHERE id='20261113190600-remove-admission-timing-18a8ff23a732be469158310b6aab30a5';
DELETE FROM schema_migrations WHERE id='20261113190559-remove-trusted-plan-tables-4f79e6850c236b51af74321a36c610dc';

DELETE FROM schema_migrations WHERE id='20261113073438-remove-unused-capability-recovery-842b529121af8cb6b818c84fa78a5024';
DROP TABLE IF EXISTS workflow_capability_recoveries;
CREATE TABLE workflow_capability_recoveries (
 run_id TEXT PRIMARY KEY REFERENCES runs(id) ON DELETE CASCADE,
 repo_id INTEGER NOT NULL REFERENCES repositories(id),
 workflow_name TEXT NOT NULL,
 sha TEXT NOT NULL,
 capability_json TEXT NOT NULL CHECK(json_valid(capability_json)),
 projected_json TEXT NOT NULL CHECK(json_valid(projected_json)),
 granted_by TEXT NOT NULL,
 granted_at INTEGER NOT NULL
);
CREATE TRIGGER workflow_capability_recoveries_immutable BEFORE UPDATE ON workflow_capability_recoveries
BEGIN SELECT RAISE(ABORT,'workflow capability recovery is immutable'); END;

DELETE FROM schema_migrations WHERE id='20261113054824-bind-workflow-capability-recovery-uid-3d13e105c1264e33bbf137990e3e1977';
`

// PostStepMeasurementDowngrade restores empty telemetry tables and historical columns only in disposable old-schema fixtures.
const PostStepMeasurementDowngrade = PostNativeNodeStepsDowngrade + PostWorkflowCapabilityUIDDowngrade + `
CREATE TABLE step_results_measurement_fixture (
    run_id TEXT NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
    burn TEXT NOT NULL,
    step TEXT NOT NULL,
    ordinal INTEGER NOT NULL,
    status TEXT NOT NULL CHECK (status IN ('passed', 'failed', 'skipped', 'timed_out')),
    exit_code INTEGER NOT NULL,
    log_start INTEGER NOT NULL CHECK (log_start >= 0),
    log_end INTEGER NOT NULL CHECK (log_end >= log_start),
    started_at INTEGER,
    finished_at INTEGER,
    recorded_at INTEGER NOT NULL,
    declared_size TEXT NOT NULL DEFAULT 'M' CHECK (declared_size IN ('S', 'M', 'L', 'XL')),
    max_rss_bytes INTEGER CHECK (max_rss_bytes >= 0),
    user_cpu_ns INTEGER CHECK (user_cpu_ns >= 0),
    system_cpu_ns INTEGER CHECK (system_cpu_ns >= 0),
    termination_json TEXT CHECK(termination_json IS NULL OR length(termination_json) <= 4096),
    memory_peak_bytes INTEGER CHECK (memory_peak_bytes >= 0),
    PRIMARY KEY (run_id, burn, step)
);
INSERT INTO step_results_measurement_fixture
 SELECT run_id,burn,step,ordinal,status,exit_code,log_start,log_end,
 started_at,finished_at,recorded_at,'M',NULL,NULL,NULL,termination_json,NULL FROM step_results;
DROP TABLE step_results;
ALTER TABLE step_results_measurement_fixture RENAME TO step_results;
CREATE INDEX step_results_order_idx ON step_results(run_id,ordinal,burn,step);
CREATE TABLE step_resource_usage (
 run_id TEXT NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
 repo_id INTEGER NOT NULL REFERENCES repositories(id),
 node_id TEXT NOT NULL,
 pod_uid TEXT NOT NULL,
 container_id TEXT NOT NULL,
 burn TEXT NOT NULL,
 step TEXT NOT NULL,
 last_observed_at INTEGER NOT NULL,
 observation_json TEXT NOT NULL CHECK(length(observation_json)<=8192),
 PRIMARY KEY(run_id,node_id,pod_uid,container_id)
);
CREATE INDEX step_resource_usage_repo_idx ON step_resource_usage(repo_id,last_observed_at DESC,run_id,node_id,pod_uid,container_id);
CREATE INDEX step_resource_usage_step_idx ON step_resource_usage(repo_id,burn,step,last_observed_at DESC);
-- The v37 receipt remains present: reconstruct its index rather than replay it.
CREATE INDEX step_resource_usage_retention_idx ON step_resource_usage(last_observed_at);

-- Purpose: Persist bounded Pod resource dispositions independently of runtime IDs.
-- Rollback: Restore the pre-upgrade database backup.

CREATE TABLE pod_resource_dispositions (
 run_id TEXT NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
 repo_id INTEGER NOT NULL REFERENCES repositories(id),
 node_id TEXT NOT NULL,
 pod_uid TEXT NOT NULL,
 burn TEXT NOT NULL,
 step TEXT NOT NULL,
 observation_json TEXT NOT NULL CHECK(length(observation_json)<=3072),
 PRIMARY KEY(run_id,node_id,pod_uid)
);

-- Purpose: Retain Workflow attempt inventory independently of Pods and metrics.
-- Rollback: Restore the pre-upgrade database backup.
CREATE TABLE resource_attempt_inventories (
 run_id TEXT PRIMARY KEY REFERENCES runs(id) ON DELETE CASCADE,
 repo_id INTEGER NOT NULL REFERENCES repositories(id),
 observation_json TEXT NOT NULL CHECK(length(observation_json)<=1024)
);
CREATE TABLE resource_attempts (
 run_id TEXT NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
 node_id TEXT NOT NULL,
 observation_json TEXT NOT NULL CHECK(length(observation_json)<=2048),
 PRIMARY KEY(run_id,node_id)
);

DELETE FROM schema_migrations WHERE id='20261113013336-remove-step-measurement-32ae7f0f09084618bddcb8f20fd0fbac';
`

// PostFragmentIndexDowngrade restores the lookup index only in disposable historical fixtures.
const PostFragmentIndexDowngrade = `
CREATE INDEX IF NOT EXISTS runs_passed_tested_sha_idx ON runs(repo_id,tested_sha) WHERE status='passed';
DELETE FROM schema_migrations WHERE id='20261113213557-remove-fragment-run-index-8c4f9bbfa3c091b95cecdb689775370b';
`

// Historical schema reconstruction for disposable migration fixtures only.
const historicalRestartTimingSchema = PostFragmentIndexDowngrade + `
CREATE TABLE IF NOT EXISTS restart_workflow_timings (
 run_id TEXT NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
 epoch INTEGER NOT NULL CHECK(epoch > 0),
 workflow_created_at INTEGER NOT NULL,
 workflow_json TEXT NOT NULL CHECK(length(workflow_json) <= 262144),
 PRIMARY KEY(run_id,epoch)
);
CREATE TABLE IF NOT EXISTS restart_step_timings (
 run_id TEXT NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
 epoch INTEGER NOT NULL CHECK(epoch > 0),
 node_id TEXT NOT NULL,
 workflow_uid TEXT NOT NULL,
 pod_uid TEXT NOT NULL,
 available INTEGER NOT NULL CHECK(available IN (0,1)),
 ordinal INTEGER NOT NULL CHECK(ordinal >= 0),
 timing_json TEXT NOT NULL CHECK(length(timing_json) <= 16384),
 PRIMARY KEY(run_id,epoch,node_id)
);
CREATE TRIGGER IF NOT EXISTS restart_workflow_timing_immutable BEFORE UPDATE ON restart_workflow_timings
BEGIN SELECT RAISE(ABORT,'restart Workflow timing is immutable'); END;
CREATE TRIGGER IF NOT EXISTS restart_step_timing_immutable BEFORE UPDATE ON restart_step_timings
WHEN NEW.run_id != OLD.run_id OR NEW.epoch != OLD.epoch OR NEW.node_id != OLD.node_id OR NEW.workflow_uid != OLD.workflow_uid
 OR (OLD.pod_uid != '' AND NEW.pod_uid != OLD.pod_uid)
 OR (OLD.available = 1 AND NEW.timing_json != OLD.timing_json)
BEGIN SELECT RAISE(ABORT,'restart step timing attempt is immutable'); END;
`

// PostExecutionTargetRemovalDowngrade restores empty custody tables only in
// disposable historical migration fixtures. Production migration only drops.
const PostExecutionTargetRemovalDowngrade = `
CREATE TABLE execution_target_bindings (
 run_id TEXT PRIMARY KEY REFERENCES runs(id) ON DELETE CASCADE,
 workflow_name TEXT NOT NULL UNIQUE,
 binding_json TEXT NOT NULL CHECK(json_valid(binding_json) AND length(binding_json)<=2097152),
 cancelled INTEGER NOT NULL DEFAULT 0 CHECK(cancelled IN (0,1)),
 admission_json TEXT NOT NULL DEFAULT '',
 cleanup_receipt TEXT NOT NULL DEFAULT ''
);
CREATE TRIGGER execution_target_binding_immutable BEFORE UPDATE OF run_id,workflow_name,binding_json ON execution_target_bindings BEGIN
 SELECT RAISE(ABORT,'immutable execution target binding');
END;
CREATE TRIGGER execution_target_cancel_monotonic BEFORE UPDATE OF cancelled ON execution_target_bindings WHEN OLD.cancelled=1 AND NEW.cancelled!=1 BEGIN
 SELECT RAISE(ABORT,'execution target cancellation cannot be revoked');
END;
CREATE TRIGGER execution_target_admission_immutable BEFORE UPDATE OF admission_json ON execution_target_bindings WHEN OLD.admission_json!='' AND OLD.admission_json!=NEW.admission_json BEGIN
 SELECT RAISE(ABORT,'immutable execution target admission');
END;
CREATE TRIGGER execution_target_cleanup_immutable BEFORE UPDATE OF cleanup_receipt ON execution_target_bindings WHEN OLD.cleanup_receipt!='' AND OLD.cleanup_receipt!=NEW.cleanup_receipt BEGIN
 SELECT RAISE(ABORT,'immutable execution target cleanup receipt');
END;
CREATE TABLE execution_target_resources (
 run_id TEXT NOT NULL REFERENCES execution_target_bindings(run_id) ON DELETE RESTRICT,
 kind TEXT NOT NULL CHECK(kind IN ('Workflow','Pod','PersistentVolumeClaim','VirtualMachineInstance')),
 namespace TEXT NOT NULL,
 name TEXT NOT NULL,
 uid TEXT NOT NULL DEFAULT '',
 object_json TEXT NOT NULL CHECK(json_valid(object_json) AND length(object_json)<=4194304),
 not_before INTEGER NOT NULL,
 PRIMARY KEY(run_id,kind,namespace,name)
);
CREATE TRIGGER execution_target_resource_immutable BEFORE UPDATE OF run_id,kind,namespace,name,object_json,not_before ON execution_target_resources BEGIN
 SELECT RAISE(ABORT,'immutable execution target create intent');
END;
CREATE TRIGGER execution_target_uid_immutable BEFORE UPDATE OF uid ON execution_target_resources WHEN OLD.uid!='' AND OLD.uid!=NEW.uid BEGIN
 SELECT RAISE(ABORT,'immutable execution target object UID');
END;
ALTER TABLE execution_target_bindings ADD COLUMN custody_digest TEXT NOT NULL DEFAULT ''
 CHECK(custody_digest='' OR (length(custody_digest)=64 AND custody_digest NOT GLOB '*[^0-9a-f]*'));

ALTER TABLE execution_target_bindings ADD COLUMN quarantine_reason TEXT NOT NULL DEFAULT ''
 CHECK(quarantine_reason IN ('','inconsistent-placement-custody'));

CREATE TRIGGER execution_target_custody_digest_immutable BEFORE UPDATE OF custody_digest ON execution_target_bindings
 WHEN OLD.custody_digest!=NEW.custody_digest BEGIN
 SELECT RAISE(ABORT,'immutable execution target custody digest');
END;
CREATE TRIGGER execution_target_quarantine_immutable BEFORE UPDATE OF quarantine_reason ON execution_target_bindings
 WHEN OLD.quarantine_reason!=NEW.quarantine_reason BEGIN
 SELECT RAISE(ABORT,'immutable execution target quarantine');
END;
DELETE FROM schema_migrations WHERE id='20261113211418-drop-execution-targets-0c8c62c219d8b3115aa50e87dde3701f';
`

// PostNativeNodeStepsDowngrade reconstructs the old step surface only in
// disposable migration fixtures. Production migrations are forward-only.
const PostNativeNodeStepsDowngrade = `
CREATE TABLE step_results_pre_nodes (
 run_id TEXT NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
 burn TEXT NOT NULL,step TEXT NOT NULL,ordinal INTEGER NOT NULL,
 status TEXT NOT NULL CHECK(status IN ('passed','failed','skipped','timed_out')),
 exit_code INTEGER NOT NULL,log_start INTEGER NOT NULL CHECK(log_start>=0),
 log_end INTEGER NOT NULL CHECK(log_end>=log_start),started_at INTEGER,finished_at INTEGER,
 recorded_at INTEGER NOT NULL,termination_json TEXT CHECK(termination_json IS NULL OR length(termination_json)<=4096),
 PRIMARY KEY(run_id,burn,step)
);
INSERT OR IGNORE INTO step_results_pre_nodes
 SELECT run_id,burn,step,ordinal,status,exit_code,0,0,started_at,finished_at,recorded_at,termination_json
 FROM step_results WHERE status IN ('passed','failed','skipped','timed_out') ORDER BY ordinal;
DROP TABLE step_results;
ALTER TABLE step_results_pre_nodes RENAME TO step_results;
CREATE INDEX step_results_order_idx ON step_results(run_id,ordinal,burn,step);
DELETE FROM schema_migrations WHERE id='20261113214401-native-node-steps-c477a88045b8945eb269fade9eb006e2';
`
