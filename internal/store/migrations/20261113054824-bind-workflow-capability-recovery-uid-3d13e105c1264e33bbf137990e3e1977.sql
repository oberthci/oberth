-- Purpose: Bind recovery authority to the original Workflow UID.
-- Rollback: Restore the verified pre-upgrade database backup.
-- Existing receipts have no UID. Discard them; never bind by observing a new object.
DROP TABLE workflow_capability_recoveries;
CREATE TABLE workflow_capability_recoveries (
 run_id TEXT PRIMARY KEY REFERENCES runs(id) ON DELETE CASCADE,
 repo_id INTEGER NOT NULL REFERENCES repositories(id),
 workflow_name TEXT NOT NULL,
 workflow_uid TEXT NOT NULL CHECK(length(trim(workflow_uid))>0),
 sha TEXT NOT NULL,
 capability_json TEXT NOT NULL CHECK(json_valid(capability_json)),
 projected_json TEXT NOT NULL CHECK(json_valid(projected_json)),
 granted_by TEXT NOT NULL,
 granted_at INTEGER NOT NULL
);
CREATE TRIGGER workflow_capability_recoveries_immutable BEFORE UPDATE ON workflow_capability_recoveries
BEGIN SELECT RAISE(ABORT,'workflow capability recovery is immutable'); END;
