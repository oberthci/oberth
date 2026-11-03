package store

const workflowCapabilityMigrationVersion = 47

// Existing technology-specific grants confer no generic Kubernetes authority.
// Operators must explicitly grant the replacement against their own RBAC.
const workflowCapabilitySchema = `
CREATE TABLE workflow_capability_grants (
 repo_id INTEGER PRIMARY KEY REFERENCES repositories(id),
 capability_json TEXT NOT NULL CHECK (json_valid(capability_json) AND length(capability_json) <= 16384),
 granted_by TEXT NOT NULL CHECK (length(granted_by) > 0),
 granted_at INTEGER NOT NULL,
 revoked_by TEXT NOT NULL DEFAULT '',
 revoked_at INTEGER,
 CHECK ((revoked_at IS NULL AND revoked_by = '') OR (revoked_at IS NOT NULL AND length(revoked_by) > 0))
);
`
