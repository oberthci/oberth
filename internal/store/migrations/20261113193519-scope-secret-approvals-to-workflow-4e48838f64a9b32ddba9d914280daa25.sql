-- Purpose: all secret approvals are cleared; re-approve. Scope new grants to workflow files.
-- Rollback: Restore the pre-upgrade database backup.
CREATE TEMP TABLE secret_access_sequence AS SELECT seq FROM sqlite_sequence WHERE name='secret_access';
DROP TABLE secret_access;
CREATE TABLE secret_access (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    repo TEXT NOT NULL,
    workflow TEXT NOT NULL CHECK (length(workflow)>0),
    step TEXT NOT NULL,
    secret TEXT NOT NULL,
    approved_by TEXT NOT NULL,
    approved_at INTEGER NOT NULL,
    revoked_by TEXT NOT NULL DEFAULT '',
    revoked_at INTEGER
);
INSERT INTO sqlite_sequence(name,seq) SELECT 'secret_access',seq FROM secret_access_sequence;
DROP TABLE secret_access_sequence;
CREATE UNIQUE INDEX secret_access_active ON secret_access(repo,workflow,step,secret) WHERE revoked_at IS NULL;
DROP TABLE grant_declarations;
CREATE TABLE grant_declarations (
    repo TEXT NOT NULL,
    workflow TEXT NOT NULL CHECK (length(workflow)>0),
    step TEXT NOT NULL,
    path TEXT NOT NULL,
    last_seen_sha TEXT NOT NULL,
    last_seen_at INTEGER NOT NULL,
    PRIMARY KEY (repo,workflow,step,path)
) WITHOUT ROWID;
