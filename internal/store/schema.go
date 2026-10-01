package store

type migration struct {
	version  int
	sql      string
	apply    migrationFunc
	rawApply connectionLevelMigration // runs outside the normal migration transaction
}

// LatestMigrationVersion returns the highest schema migration version this
// binary will apply. The running server's version command reports it so the
// installer can detect pending migrations before upgrading.
func LatestMigrationVersion() int { return latestMigrationVersion }

const (
	latestMigrationVersion = 17
	// oberthSchemaIdentity is the fresh-deployment default. Existing deployments
	// supply their original immutable identity through ProtocolConfig; bootstrap
	// SQL and legacy schema validation use that exact configured operand.
	oberthSchemaIdentity  = "oberth-schema-v1"
	createMigrationLedger = `
CREATE TABLE IF NOT EXISTS schema_migrations (
    version INTEGER PRIMARY KEY,
    applied_at INTEGER NOT NULL
)`
)

var migrations = []migration{
	{
		version: 1,
		sql: `
CREATE TABLE schema_identity (
    singleton INTEGER PRIMARY KEY CHECK (singleton = 1),
    identity TEXT NOT NULL CHECK (identity = 'oberth-schema-v1')
);
INSERT INTO schema_identity(singleton, identity)
VALUES(1, 'oberth-schema-v1');

CREATE TABLE upstreams (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    name TEXT NOT NULL UNIQUE,
    kind TEXT NOT NULL,
    base_url TEXT NOT NULL,
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL
);

CREATE TABLE repositories (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    name TEXT NOT NULL UNIQUE,
    upstream_id INTEGER NOT NULL REFERENCES upstreams(id),
    default_branch TEXT NOT NULL,
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL
);

CREATE TABLE runs (
    queue_sequence INTEGER PRIMARY KEY AUTOINCREMENT,
    id TEXT NOT NULL UNIQUE,
    repo_id INTEGER NOT NULL REFERENCES repositories(id),
    ref_kind TEXT NOT NULL CHECK (ref_kind IN ('branch', 'tag')),
    ref TEXT NOT NULL,
    sha TEXT NOT NULL,
    actor TEXT NOT NULL,
    release INTEGER NOT NULL DEFAULT 0 CHECK (release IN (0, 1)),
    trigger TEXT NOT NULL,
    phase TEXT NOT NULL DEFAULT 'queued',
    job_name TEXT NOT NULL DEFAULT '',
    tested_sha TEXT NOT NULL DEFAULT '',
    base_sha TEXT NOT NULL DEFAULT '',
    failed_burn TEXT NOT NULL DEFAULT '',
    failed_step TEXT NOT NULL DEFAULT '',
    error TEXT NOT NULL DEFAULT '',
    status TEXT NOT NULL CHECK (status IN ('queued', 'running', 'passed', 'failed', 'interrupted')),
    reason TEXT NOT NULL DEFAULT '',
    superseded_by TEXT NOT NULL DEFAULT '',
    queued_at INTEGER NOT NULL,
    started_at INTEGER,
    finished_at INTEGER,
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL
);
CREATE INDEX runs_fifo_idx ON runs(status, queue_sequence);
CREATE INDEX runs_ref_idx ON runs(repo_id, ref, queue_sequence DESC);
CREATE INDEX runs_sha_idx ON runs(repo_id, sha, queue_sequence DESC);

CREATE TABLE step_results (
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
    PRIMARY KEY (run_id, burn, step)
);
CREATE INDEX step_results_order_idx ON step_results(run_id, ordinal, burn, step);

CREATE TABLE promotions (
    sequence INTEGER PRIMARY KEY AUTOINCREMENT,
    id TEXT NOT NULL UNIQUE,
    repo_id INTEGER NOT NULL REFERENCES repositories(id),
    source_branch TEXT NOT NULL,
    source_sha TEXT NOT NULL,
    target_ref TEXT NOT NULL,
    previous_sha TEXT NOT NULL,
    result_sha TEXT NOT NULL,
    actor TEXT NOT NULL,
    status TEXT NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'passed', 'failed', 'interrupted')),
    run_id TEXT NOT NULL DEFAULT '',
    error TEXT NOT NULL DEFAULT '',
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL
);
CREATE INDEX promotions_repo_idx ON promotions(repo_id, sequence DESC);
CREATE UNIQUE INDEX promotions_run_idx
    ON promotions(run_id) WHERE run_id != '';

CREATE TABLE issues (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    repo_id INTEGER REFERENCES repositories(id),
    kind TEXT NOT NULL CHECK (kind IN ('manual', 'ci')),
    branch TEXT NOT NULL DEFAULT '',
    title TEXT NOT NULL,
    body TEXT NOT NULL,
    state TEXT NOT NULL CHECK (state IN ('open', 'closed')),
    occurrences INTEGER NOT NULL DEFAULT 1 CHECK (occurrences > 0),
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL,
    closed_at INTEGER,
    ci_origin TEXT NOT NULL DEFAULT '' CHECK (ci_origin IN ('', 'branch', 'promotion')),
    ci_work_sequence INTEGER NOT NULL DEFAULT 0 CHECK (ci_work_sequence >= 0),
    ci_work_id TEXT NOT NULL DEFAULT '',
    CHECK ((kind = 'ci' AND repo_id IS NOT NULL) OR (kind = 'manual' AND repo_id IS NULL))
);
CREATE UNIQUE INDEX one_open_ci_issue_idx
    ON issues(repo_id, branch)
    WHERE kind = 'ci' AND state = 'open';
CREATE INDEX manual_issues_page_idx ON issues(kind, repo_id, state, id DESC);
CREATE INDEX issues_page_idx ON issues(repo_id, kind, state, id DESC);

CREATE TABLE issue_locks (
    issue_id INTEGER PRIMARY KEY REFERENCES issues(id) ON DELETE CASCADE,
    owner TEXT NOT NULL,
    acquired_at INTEGER NOT NULL,
    expires_at INTEGER NOT NULL
);
CREATE INDEX issue_locks_expiry_idx ON issue_locks(expires_at);

CREATE TABLE audit_actions (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    actor TEXT NOT NULL,
    action TEXT NOT NULL,
    resource_type TEXT NOT NULL,
    resource_id TEXT NOT NULL,
    details TEXT NOT NULL,
    created_at INTEGER NOT NULL
);
CREATE INDEX audit_actions_page_idx ON audit_actions(id DESC);

CREATE TABLE token_credentials (
    id TEXT PRIMARY KEY,
    name TEXT NOT NULL,
    digest BLOB NOT NULL UNIQUE CHECK (length(digest) = 32),
    created_at INTEGER NOT NULL,
    last_used_at INTEGER,
    revoked_at INTEGER,
    activated_at INTEGER
);

CREATE TABLE uplinks (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    fingerprint TEXT NOT NULL UNIQUE,
    identity TEXT NOT NULL,
    token_credential_id TEXT NOT NULL REFERENCES token_credentials(id),
    auth_actor TEXT NOT NULL,
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL
);
CREATE UNIQUE INDEX uplinks_token_credential_idx ON uplinks(token_credential_id);

CREATE TABLE run_cancellations (
    run_id TEXT PRIMARY KEY REFERENCES runs(id) ON DELETE CASCADE,
    job_name TEXT NOT NULL DEFAULT '',
    superseded_by TEXT NOT NULL DEFAULT '',
    reason TEXT NOT NULL CHECK (reason IN ('superseded', 'owner_restart')),
    created_at INTEGER NOT NULL,
    completed_at INTEGER
);
CREATE INDEX run_cancellations_pending_idx
    ON run_cancellations(completed_at, created_at, run_id);

CREATE TABLE receive_events (
    id TEXT PRIMARY KEY,
    actor TEXT NOT NULL,
    repo_id INTEGER NOT NULL REFERENCES repositories(id),
    ref_kind TEXT NOT NULL CHECK (ref_kind IN ('branch', 'tag')),
    ref TEXT NOT NULL,
    old_sha TEXT NOT NULL DEFAULT '',
    object_sha TEXT NOT NULL DEFAULT '',
    commit_sha TEXT NOT NULL DEFAULT '',
    outcome TEXT NOT NULL,
    run_id TEXT NOT NULL DEFAULT '',
    created_at INTEGER NOT NULL
);
CREATE UNIQUE INDEX receive_events_run_idx
    ON receive_events(run_id) WHERE run_id != '';
CREATE INDEX receive_events_repo_idx ON receive_events(repo_id, created_at, id);

CREATE TABLE pending_token_credentials (
    token_credential_id TEXT PRIMARY KEY REFERENCES token_credentials(id) ON DELETE CASCADE,
    created_at INTEGER NOT NULL
);

CREATE TABLE publications (
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
    CHECK (run_id IS NOT NULL OR promotion_id IS NOT NULL)
);
CREATE UNIQUE INDEX publications_run_idx
    ON publications(run_id) WHERE run_id IS NOT NULL;
CREATE UNIQUE INDEX publications_promotion_idx
    ON publications(promotion_id) WHERE promotion_id IS NOT NULL;
CREATE INDEX publications_pending_idx ON publications(status, sequence);

CREATE TABLE ci_issue_work (
    sequence INTEGER PRIMARY KEY AUTOINCREMENT,
    repo_id INTEGER NOT NULL REFERENCES repositories(id),
    branch TEXT NOT NULL,
    origin TEXT NOT NULL CHECK (origin IN ('branch', 'promotion')),
    run_id TEXT REFERENCES runs(id),
    promotion_id TEXT REFERENCES promotions(id),
    actor TEXT NOT NULL,
    created_at INTEGER NOT NULL,
    CHECK (
        (origin = 'branch' AND run_id IS NOT NULL AND promotion_id IS NULL)
        OR (origin = 'promotion' AND run_id IS NULL AND promotion_id IS NOT NULL)
    )
);
CREATE UNIQUE INDEX ci_issue_work_run_idx
    ON ci_issue_work(run_id) WHERE run_id IS NOT NULL;
CREATE UNIQUE INDEX ci_issue_work_promotion_idx
    ON ci_issue_work(promotion_id) WHERE promotion_id IS NOT NULL;
CREATE INDEX ci_issue_work_branch_idx
    ON ci_issue_work(repo_id, branch, sequence);

CREATE TABLE ci_issue_projections (
    repo_id INTEGER NOT NULL REFERENCES repositories(id),
    branch TEXT NOT NULL,
    origin TEXT NOT NULL CHECK (origin IN ('branch', 'promotion')),
    work_sequence INTEGER NOT NULL CHECK (work_sequence > 0),
    work_id TEXT NOT NULL,
    outcome TEXT NOT NULL CHECK (outcome IN ('passed', 'failed')),
    title TEXT NOT NULL,
    body TEXT NOT NULL,
    actor TEXT NOT NULL,
    updated_at INTEGER NOT NULL,
    PRIMARY KEY (repo_id, branch, origin)
);
CREATE INDEX ci_issue_projection_failure_idx
    ON ci_issue_projections(repo_id, branch, outcome, work_sequence DESC);

CREATE TRIGGER schema_identity_no_insert
BEFORE INSERT ON schema_identity
BEGIN
    SELECT RAISE(ABORT, 'schema identity is immutable');
END;
CREATE TRIGGER schema_identity_no_update
BEFORE UPDATE ON schema_identity
BEGIN
    SELECT RAISE(ABORT, 'schema identity is immutable');
END;
CREATE TRIGGER schema_identity_no_delete
BEFORE DELETE ON schema_identity
BEGIN
    SELECT RAISE(ABORT, 'schema identity is immutable');
END;

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
BEGIN
    SELECT RAISE(ABORT, 'promotion transition is immutable');
END;
CREATE TRIGGER promotions_no_delete
BEFORE DELETE ON promotions
BEGIN
    SELECT RAISE(ABORT, 'promotions are append-only');
END;
CREATE TRIGGER audit_actions_no_update
BEFORE UPDATE ON audit_actions
BEGIN
    SELECT RAISE(ABORT, 'audit actions are append-only');
END;
CREATE TRIGGER audit_actions_no_delete
BEFORE DELETE ON audit_actions
BEGIN
    SELECT RAISE(ABORT, 'audit actions are append-only');
END;

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
    AND NEW.actor = OLD.actor
    AND NEW.created_at = OLD.created_at
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
BEGIN
    SELECT RAISE(ABORT, 'publication transition is immutable');
END;
CREATE TRIGGER publications_no_delete
BEFORE DELETE ON publications
BEGIN
    SELECT RAISE(ABORT, 'publications are append-only');
END;

CREATE TRIGGER ci_issue_work_no_update
BEFORE UPDATE ON ci_issue_work
BEGIN
    SELECT RAISE(ABORT, 'CI issue work is append-only');
END;
CREATE TRIGGER ci_issue_work_no_delete
BEFORE DELETE ON ci_issue_work
BEGIN
    SELECT RAISE(ABORT, 'CI issue work is append-only');
END;
CREATE TRIGGER ci_issue_projection_monotonic
BEFORE UPDATE ON ci_issue_projections
WHEN NEW.repo_id != OLD.repo_id
  OR NEW.branch != OLD.branch
  OR NEW.origin != OLD.origin
  OR NEW.work_sequence <= OLD.work_sequence
BEGIN
    SELECT RAISE(ABORT, 'CI issue projection must advance monotonically');
END;
`,
	},
	{
		version: 2,
		apply:   migrateAuditChainV2,
	},
	{
		version: 3,
		sql: `
ALTER TABLE step_results
    ADD COLUMN declared_size TEXT NOT NULL DEFAULT 'M'
    CHECK (declared_size IN ('S', 'M', 'L', 'XL'));
ALTER TABLE step_results ADD COLUMN max_rss_bytes INTEGER NOT NULL DEFAULT 0 CHECK (max_rss_bytes >= 0);
ALTER TABLE step_results ADD COLUMN user_cpu_ns INTEGER NOT NULL DEFAULT 0 CHECK (user_cpu_ns >= 0);
ALTER TABLE step_results ADD COLUMN system_cpu_ns INTEGER NOT NULL DEFAULT 0 CHECK (system_cpu_ns >= 0);`,
	},
	{
		version: 4,
		sql: `
CREATE TABLE trusted_plans (
    sequence INTEGER PRIMARY KEY AUTOINCREMENT,
    id TEXT NOT NULL UNIQUE,
    repo_id INTEGER NOT NULL REFERENCES repositories(id),
    upstream_id INTEGER NOT NULL REFERENCES upstreams(id),
    source_ref TEXT NOT NULL,
    source_sha TEXT NOT NULL,
    target_ref TEXT NOT NULL,
    base_sha TEXT NOT NULL,
    result_sha TEXT NOT NULL,
    green_run_id TEXT NOT NULL REFERENCES runs(id),
    plan_run_id TEXT NOT NULL UNIQUE REFERENCES runs(id),
    promotion_id TEXT NOT NULL DEFAULT '',
    apply_id TEXT NOT NULL DEFAULT '',
    apply_enqueue_error TEXT NOT NULL DEFAULT '' CHECK (length(apply_enqueue_error) <= 4096),
    backend_identity TEXT NOT NULL,
    backend_key TEXT NOT NULL,
    tool_digest TEXT NOT NULL CHECK (length(tool_digest) = 64),
    lock_digest TEXT NOT NULL CHECK (length(lock_digest) = 64),
    config_digest TEXT NOT NULL CHECK (length(config_digest) = 64),
    artifact_digest TEXT NOT NULL DEFAULT '' CHECK (artifact_digest = '' OR length(artifact_digest) = 64),
    artifact_size INTEGER NOT NULL DEFAULT 0 CHECK (artifact_size >= 0 AND artifact_size <= 16777216),
    transit_ciphertext TEXT NOT NULL DEFAULT '' CHECK (length(transit_ciphertext) <= 33554432),
    actor TEXT NOT NULL,
    status TEXT NOT NULL CHECK (status IN ('authorized', 'ready', 'attached', 'applying', 'consumed', 'failed', 'expired')),
    error TEXT NOT NULL DEFAULT '',
    created_at INTEGER NOT NULL,
    expires_at INTEGER NOT NULL,
    consumed_at INTEGER,
    updated_at INTEGER NOT NULL,
    CHECK (result_sha = source_sha),
    CHECK (expires_at > created_at),
    CHECK ((artifact_digest = '' AND artifact_size = 0 AND transit_ciphertext = '') OR
           (artifact_digest != '' AND artifact_size > 0 AND
            (transit_ciphertext != '' OR status IN ('consumed', 'failed', 'expired')))),
    CHECK (status NOT IN ('ready', 'attached', 'applying') OR transit_ciphertext != '')
);
CREATE UNIQUE INDEX trusted_plans_active_backend_idx
    ON trusted_plans(backend_key)
    WHERE status IN ('authorized', 'ready', 'attached', 'applying');
CREATE UNIQUE INDEX trusted_plans_promotion_idx
    ON trusted_plans(promotion_id) WHERE promotion_id != '';
CREATE UNIQUE INDEX trusted_plans_apply_idx
    ON trusted_plans(apply_id) WHERE apply_id != '';
CREATE INDEX trusted_plans_repo_idx ON trusted_plans(repo_id, sequence DESC);
CREATE INDEX trusted_plans_expiry_idx ON trusted_plans(status, expires_at);

CREATE TABLE trusted_applies (
    sequence INTEGER PRIMARY KEY AUTOINCREMENT,
    id TEXT NOT NULL UNIQUE,
    plan_id TEXT NOT NULL UNIQUE REFERENCES trusted_plans(id),
    promotion_id TEXT NOT NULL UNIQUE REFERENCES promotions(id),
    repo_id INTEGER NOT NULL REFERENCES repositories(id),
    target_ref TEXT NOT NULL,
    sha TEXT NOT NULL,
    run_id TEXT NOT NULL UNIQUE REFERENCES runs(id),
    artifact_digest TEXT NOT NULL CHECK (length(artifact_digest) = 64),
    backend_key TEXT NOT NULL,
    actor TEXT NOT NULL,
    status TEXT NOT NULL CHECK (status IN ('queued', 'running', 'passed', 'failed', 'interrupted')),
    error TEXT NOT NULL DEFAULT '',
    created_at INTEGER NOT NULL,
    started_at INTEGER,
    finished_at INTEGER,
    updated_at INTEGER NOT NULL
);
CREATE INDEX trusted_applies_repo_idx ON trusted_applies(repo_id, sequence DESC);

CREATE TRIGGER trusted_plans_guard_update
BEFORE UPDATE ON trusted_plans
WHEN NEW.sequence != OLD.sequence
  OR NEW.id != OLD.id
  OR NEW.repo_id != OLD.repo_id
  OR NEW.upstream_id != OLD.upstream_id
  OR NEW.source_ref != OLD.source_ref
  OR NEW.source_sha != OLD.source_sha
  OR NEW.target_ref != OLD.target_ref
  OR NEW.base_sha != OLD.base_sha
  OR NEW.result_sha != OLD.result_sha
  OR NEW.green_run_id != OLD.green_run_id
  OR NEW.plan_run_id != OLD.plan_run_id
  OR NEW.backend_identity != OLD.backend_identity
  OR NEW.backend_key != OLD.backend_key
  OR NEW.tool_digest != OLD.tool_digest
  OR NEW.lock_digest != OLD.lock_digest
  OR NEW.config_digest != OLD.config_digest
  OR NEW.actor != OLD.actor
  OR NEW.created_at != OLD.created_at
  OR NEW.expires_at != OLD.expires_at
  OR NOT (
      (
        OLD.status = 'authorized' AND NEW.status = 'authorized'
        AND OLD.artifact_digest = '' AND OLD.artifact_size = 0 AND OLD.transit_ciphertext = ''
        AND NEW.artifact_digest != '' AND NEW.artifact_size > 0 AND NEW.transit_ciphertext != ''
        AND NEW.promotion_id = OLD.promotion_id AND NEW.apply_id = OLD.apply_id
        AND NEW.apply_enqueue_error = OLD.apply_enqueue_error
        AND NEW.error = OLD.error AND NEW.consumed_at IS OLD.consumed_at
      )
      OR (
        OLD.status = 'authorized' AND NEW.status = 'ready'
        AND NEW.artifact_digest = OLD.artifact_digest AND NEW.artifact_size = OLD.artifact_size
        AND NEW.transit_ciphertext = OLD.transit_ciphertext
        AND NEW.promotion_id = OLD.promotion_id AND NEW.apply_id = OLD.apply_id
        AND NEW.apply_enqueue_error = OLD.apply_enqueue_error
        AND NEW.error = '' AND NEW.consumed_at IS OLD.consumed_at
      )
      OR (
        OLD.status = 'authorized' AND NEW.status IN ('failed', 'expired')
        AND NEW.artifact_digest = OLD.artifact_digest AND NEW.artifact_size = OLD.artifact_size
        AND NEW.transit_ciphertext = ''
        AND NEW.promotion_id = OLD.promotion_id AND NEW.apply_id = OLD.apply_id
        AND NEW.apply_enqueue_error = OLD.apply_enqueue_error
        AND NEW.error != '' AND NEW.consumed_at IS OLD.consumed_at
      )
      OR (
        OLD.status = 'ready' AND NEW.status = 'attached'
        AND OLD.promotion_id = '' AND NEW.promotion_id != ''
        AND NEW.apply_id = OLD.apply_id AND NEW.error = OLD.error
        AND NEW.apply_enqueue_error = OLD.apply_enqueue_error
        AND NEW.artifact_digest = OLD.artifact_digest AND NEW.artifact_size = OLD.artifact_size
        AND NEW.transit_ciphertext != '' AND NEW.transit_ciphertext != OLD.transit_ciphertext
        AND NEW.consumed_at IS OLD.consumed_at
      )
      OR (
        OLD.status = 'ready' AND NEW.status IN ('failed', 'expired')
        AND NEW.promotion_id = OLD.promotion_id AND NEW.apply_id = OLD.apply_id
        AND NEW.apply_enqueue_error = OLD.apply_enqueue_error
        AND NEW.artifact_digest = OLD.artifact_digest AND NEW.artifact_size = OLD.artifact_size
        AND NEW.transit_ciphertext = ''
        AND NEW.error != '' AND NEW.consumed_at IS OLD.consumed_at
      )
      OR (
        OLD.status = 'attached' AND NEW.status = 'attached'
        AND NEW.promotion_id = OLD.promotion_id AND NEW.apply_id = OLD.apply_id
        AND OLD.apply_enqueue_error = '' AND NEW.apply_enqueue_error != ''
        AND NEW.artifact_digest = OLD.artifact_digest AND NEW.artifact_size = OLD.artifact_size
        AND NEW.transit_ciphertext = OLD.transit_ciphertext
        AND NEW.error = OLD.error AND NEW.consumed_at IS OLD.consumed_at
      )
      OR (
        OLD.status = 'attached' AND NEW.status = 'applying'
        AND OLD.apply_id = '' AND NEW.apply_id != '' AND NEW.promotion_id = OLD.promotion_id
        AND NEW.apply_enqueue_error = ''
        AND NEW.artifact_digest = OLD.artifact_digest AND NEW.artifact_size = OLD.artifact_size
        AND NEW.transit_ciphertext = OLD.transit_ciphertext
        AND NEW.error = OLD.error AND NEW.consumed_at IS OLD.consumed_at
      )
      OR (
        OLD.status = 'attached' AND NEW.status = 'failed'
        AND NEW.promotion_id = OLD.promotion_id AND NEW.apply_id = OLD.apply_id
        AND NEW.apply_enqueue_error = OLD.apply_enqueue_error
        AND NEW.artifact_digest = OLD.artifact_digest AND NEW.artifact_size = OLD.artifact_size
        AND NEW.transit_ciphertext = ''
        AND NEW.error != '' AND NEW.consumed_at IS OLD.consumed_at
      )
      OR (
        OLD.status = 'applying' AND NEW.status = 'consumed'
        AND NEW.promotion_id = OLD.promotion_id AND NEW.apply_id = OLD.apply_id
        AND NEW.apply_enqueue_error = OLD.apply_enqueue_error
        AND NEW.artifact_digest = OLD.artifact_digest AND NEW.artifact_size = OLD.artifact_size
        AND NEW.transit_ciphertext = ''
        AND NEW.error = OLD.error AND OLD.consumed_at IS NULL AND NEW.consumed_at IS NOT NULL
      )
  )
BEGIN
    SELECT RAISE(ABORT, 'trusted plan transition is immutable');
END;
CREATE TRIGGER trusted_plans_no_delete
BEFORE DELETE ON trusted_plans
BEGIN
    SELECT RAISE(ABORT, 'trusted plans are append-only');
END;

CREATE TRIGGER trusted_applies_guard_update
BEFORE UPDATE ON trusted_applies
WHEN NEW.sequence != OLD.sequence
  OR NEW.id != OLD.id
  OR NEW.plan_id != OLD.plan_id
  OR NEW.promotion_id != OLD.promotion_id
  OR NEW.repo_id != OLD.repo_id
  OR NEW.target_ref != OLD.target_ref
  OR NEW.sha != OLD.sha
  OR NEW.run_id != OLD.run_id
  OR NEW.artifact_digest != OLD.artifact_digest
  OR NEW.backend_key != OLD.backend_key
  OR NEW.actor != OLD.actor
  OR NEW.created_at != OLD.created_at
  OR NOT (
      (
        OLD.status = 'queued' AND NEW.status = 'running'
        AND OLD.error = '' AND NEW.error = ''
        AND OLD.started_at IS NULL AND NEW.started_at IS NOT NULL
        AND OLD.finished_at IS NULL AND NEW.finished_at IS NULL
      )
      OR (
        OLD.status = 'queued' AND NEW.status = 'passed'
        AND NEW.error = ''
        AND NEW.started_at IS OLD.started_at
        AND OLD.finished_at IS NULL AND NEW.finished_at IS NOT NULL
      )
      OR (
        OLD.status = 'queued' AND NEW.status IN ('failed', 'interrupted')
        AND NEW.error != ''
        AND NEW.started_at IS OLD.started_at
        AND OLD.finished_at IS NULL AND NEW.finished_at IS NOT NULL
      )
      OR (
        OLD.status = 'running' AND NEW.status = 'passed'
        AND NEW.error = ''
        AND NEW.started_at = OLD.started_at
        AND OLD.finished_at IS NULL AND NEW.finished_at IS NOT NULL
      )
      OR (
        OLD.status = 'running' AND NEW.status IN ('failed', 'interrupted')
        AND NEW.error != ''
        AND NEW.started_at = OLD.started_at
        AND OLD.finished_at IS NULL AND NEW.finished_at IS NOT NULL
      )
  )
BEGIN
    SELECT RAISE(ABORT, 'trusted apply transition is immutable');
END;
CREATE TRIGGER trusted_applies_no_delete
BEFORE DELETE ON trusted_applies
BEGIN
    SELECT RAISE(ABORT, 'trusted applies are append-only');
END;
`,
	},
	{
		version: 5,
		sql: `
CREATE TABLE secret_access (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    repo TEXT NOT NULL,
    step TEXT NOT NULL,
    secret TEXT NOT NULL,
    approved_by TEXT NOT NULL,
    approved_at INTEGER NOT NULL,
    revoked_by TEXT NOT NULL DEFAULT '',
    revoked_at INTEGER
);
CREATE UNIQUE INDEX secret_access_active
    ON secret_access(repo, step, secret) WHERE revoked_at IS NULL;
`,
	},
	{
		version: 6,
		sql:     `ALTER TABLE uplinks ADD COLUMN admin INTEGER NOT NULL DEFAULT 0 CHECK (admin IN (0, 1));`,
	},
	{
		version: 7,
		sql:     `ALTER TABLE upstreams ADD COLUMN key_name TEXT NOT NULL DEFAULT '';`,
	},
	{
		version: 8,
		sql:     `ALTER TABLE runs ADD COLUMN credentialed INTEGER NOT NULL DEFAULT 0 CHECK (credentialed IN (0, 1));`,
	},
	{
		version: 9,
		sql: `CREATE TABLE schedule_fires (
    repo TEXT NOT NULL,
    entry TEXT NOT NULL,
    fired_at INTEGER NOT NULL,
    outcome TEXT NOT NULL,
    PRIMARY KEY (repo, entry)
) WITHOUT ROWID;`,
	},
	{
		// v10 shipped in v0.13.25/v0.13.26 as a ledger-only no-op and is
		// RECORDED on live databases. Its body must never be replaced: the
		// migration runner skips any version <= the recorded database
		// version, so a rewritten body silently never runs on exactly the
		// databases that need it (reproduced against the live lineage in
		// TestMigrationLiveLineageAppliesRebuildAfterRecordedV10). New
		// schema work appends new versions; it never edits shipped ones.
		version: 10,
		sql: `-- Phase 1 of org-qualified identity (#245): reserved-name guards and
-- upstream namespace disjointness enforced in code. Compound unique on
-- (upstream_id, name) deferred to canonical persistence (G3); grants,
-- schedule_fires, and cache paths must key on the qualified form before
-- same-name repos across upstreams can be admitted.
SELECT 1`,
	},
	{
		version:  11,
		rawApply: migrateV11CanonicalPersistence,
	},
	{
		version:  12,
		rawApply: migrateV12SecretAccessQualifiedNames,
	},
	{
		version: 13,
		sql: `CREATE TABLE vm_executions (
    run_id TEXT PRIMARY KEY REFERENCES runs(id),
    intent_json TEXT NOT NULL,
    spec_identity TEXT NOT NULL,
    instance_json TEXT NOT NULL DEFAULT '',
    submitted INTEGER NOT NULL DEFAULT 0 CHECK (submitted IN (0,1)),
    rejected INTEGER NOT NULL DEFAULT 0 CHECK (rejected IN (0,1)),
    cleaning INTEGER NOT NULL DEFAULT 0 CHECK (cleaning IN (0,1)),
    cleaned INTEGER NOT NULL DEFAULT 0 CHECK (cleaned IN (0,1)),
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL,
    CHECK (cleaned=0 OR cleaning=1),
    CHECK (instance_json='' OR submitted=1),
    CHECK (rejected=0 OR (submitted=1 AND instance_json='')),
    CHECK (cleaned=0 OR submitted=0 OR rejected=1 OR instance_json!='')
);
CREATE UNIQUE INDEX vm_execution_capacity ON vm_executions(cleaned) WHERE cleaned=0;
`,
	},
	{
		version: 14,
		sql: `CREATE TABLE vm_capacity_slots (
    slot INTEGER PRIMARY KEY CHECK (slot=1),
    run_id TEXT NOT NULL UNIQUE REFERENCES runs(id)
);
INSERT INTO vm_capacity_slots(slot,run_id) SELECT 1,run_id FROM vm_executions WHERE cleaned=0;
CREATE TABLE vm_suite_executions (
    run_id TEXT PRIMARY KEY REFERENCES runs(id),
    plan_json TEXT NOT NULL,
    plan_identity TEXT NOT NULL,
    submitted INTEGER NOT NULL DEFAULT 0 CHECK (submitted IN (0,1)),
    cleaning INTEGER NOT NULL DEFAULT 0 CHECK (cleaning IN (0,1)),
    cleaned INTEGER NOT NULL DEFAULT 0 CHECK (cleaned IN (0,1)),
    failure TEXT NOT NULL DEFAULT '',
    attempt_json TEXT NOT NULL DEFAULT '',
    receipt_json TEXT NOT NULL DEFAULT '',
    endpoint_json TEXT NOT NULL DEFAULT '',
    restart_json TEXT NOT NULL DEFAULT '',
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL,
    CHECK (cleaned=0 OR cleaning=1),
    CHECK (attempt_json='' OR submitted=1),
    CHECK (receipt_json='' OR attempt_json!='')
);
CREATE TABLE vm_suite_resources (
    run_id TEXT NOT NULL REFERENCES vm_suite_executions(run_id),
    resource_key TEXT NOT NULL,
    intent_json TEXT NOT NULL,
    receipt_json TEXT NOT NULL DEFAULT '',
    uid TEXT NOT NULL DEFAULT '',
    submitted INTEGER NOT NULL DEFAULT 0 CHECK (submitted IN (0,1)),
    rejected INTEGER NOT NULL DEFAULT 0 CHECK (rejected IN (0,1)),
    cleaning INTEGER NOT NULL DEFAULT 0 CHECK (cleaning IN (0,1)),
    cleaned INTEGER NOT NULL DEFAULT 0 CHECK (cleaned IN (0,1)),
    PRIMARY KEY (run_id,resource_key),
    CHECK (cleaned=0 OR cleaning=1),
    CHECK ((receipt_json='' AND uid='') OR (receipt_json!='' AND uid!='' AND submitted=1)),
    CHECK (rejected=0 OR (submitted=1 AND uid='')),
    CHECK (cleaned=0 OR submitted=0 OR rejected=1 OR uid!='')
);
CREATE UNIQUE INDEX vm_suite_resource_uid ON vm_suite_resources(run_id,uid) WHERE uid!='';
`,
	},
	{
		version: 15,
		sql:     `ALTER TABLE vm_suite_executions ADD COLUMN attach_json TEXT NOT NULL DEFAULT '';`,
	},
	{
		// The repository-declared concurrency group a run was admitted under
		// (#658), recorded once the scheduler has read it from the run's own
		// immutable checkout. Empty means no group. The CHECK mirrors
		// argoworkflow.ValidConcurrencyGroup so no other spelling can be stored.
		version: 16,
		sql: `ALTER TABLE runs ADD COLUMN concurrency_group TEXT NOT NULL DEFAULT ''
    CHECK (concurrency_group = '' OR (length(concurrency_group) BETWEEN 1 AND 63
        AND concurrency_group NOT GLOB '*[^a-z0-9-]*'));`,
	},
	{
		// v17: Allow publish retry (#696). Two schema changes:
		//
		// 1. Publication guard: permit failed → pending transition so a
		//    publishing-failed run can be retried without re-running burns.
		//
		// 2. CI issue projection: permit same-sequence updates when the
		//    outcome transitions from failed to passed, so a retry of the
		//    same run's publication can close the publish-failure issue.
		version: 17,
		sql: `DROP TRIGGER publications_guard_update;
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
)
BEGIN
    SELECT RAISE(ABORT, 'publication transition is immutable');
END;

DROP TRIGGER ci_issue_projection_monotonic;
CREATE TRIGGER ci_issue_projection_monotonic
BEFORE UPDATE ON ci_issue_projections
WHEN (NEW.repo_id != OLD.repo_id
   OR NEW.branch != OLD.branch
   OR NEW.origin != OLD.origin
   OR NEW.work_sequence < OLD.work_sequence)
  AND NOT (
    NEW.work_sequence = OLD.work_sequence
    AND NEW.repo_id = OLD.repo_id
    AND NEW.branch = OLD.branch
    AND NEW.origin = OLD.origin
    AND OLD.outcome = 'failed'
    AND NEW.outcome = 'passed'
  )
BEGIN
    SELECT RAISE(ABORT, 'CI issue projection must advance monotonically');
END;`,
	},
}
