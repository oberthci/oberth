package store

const resourceCeilingMigrationVersion = 49

// Copy historical ceilings exactly, including grant provenance and revocations.
// Absent and revoked grants retain the default ceiling for fresh submissions.
const resourceCeilingSchema = `CREATE TABLE resource_ceilings (
 repo_id INTEGER PRIMARY KEY REFERENCES repositories(id) ON DELETE CASCADE,
 resources_json TEXT NOT NULL CHECK(json_valid(resources_json) AND length(resources_json) <= 4096),
 granted_by TEXT NOT NULL,
 granted_at INTEGER NOT NULL,
 revoked_by TEXT NOT NULL DEFAULT '',
 revoked_at INTEGER,
 CHECK ((revoked_at IS NULL AND revoked_by = '') OR (revoked_at IS NOT NULL AND revoked_by != ''))
);
INSERT INTO resource_ceilings(repo_id, resources_json, granted_by, granted_at, revoked_by, revoked_at)
SELECT repo_id,
 CASE max_size WHEN 'XL' THEN '{"requests":{"cpu":"8","memory":"16Gi","ephemeral-storage":"32Gi"},"limits":{"cpu":"8","memory":"16Gi","ephemeral-storage":"32Gi"}}'
 ELSE '{"requests":{"cpu":"4","memory":"8Gi","ephemeral-storage":"16Gi"},"limits":{"cpu":"4","memory":"8Gi","ephemeral-storage":"16Gi"}}' END,
 granted_by, granted_at, revoked_by, revoked_at FROM size_grants;`
