CREATE TABLE kubevirt_grants (
    repo_id INTEGER PRIMARY KEY REFERENCES repositories(id) ON DELETE CASCADE,
    granted_by TEXT NOT NULL,
    granted_at INTEGER NOT NULL,
    revoked_by TEXT NOT NULL DEFAULT '',
    revoked_at INTEGER,
    CHECK ((revoked_at IS NULL AND revoked_by = '') OR (revoked_at IS NOT NULL AND revoked_by != ''))
);