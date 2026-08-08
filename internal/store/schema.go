package store

type migration struct {
	version int
	sql     string
	apply   migrationFunc
}

const (
	latestMigrationVersion = 2
	// oberthSchemaIdentity is a compatibility-frozen opaque literal: every
	// deployed database carries this exact identity row and a CHECK constraint
	// on it. Renaming it is a breaking schema migration, not a string cleanup.
	oberthSchemaIdentity  = "cloudtaser-oberth-schema-v1"
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
    identity TEXT NOT NULL CHECK (identity = 'cloudtaser-oberth-schema-v1')
);
INSERT INTO schema_identity(singleton, identity)
VALUES(1, 'cloudtaser-oberth-schema-v1');

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
}
