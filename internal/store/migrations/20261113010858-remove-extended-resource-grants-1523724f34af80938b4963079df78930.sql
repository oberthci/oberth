-- Purpose: Retire extended-resource grant settings while preserving operator API authority and audit history.
-- Rollback: Restore the verified pre-upgrade database backup.
-- Retain the exact operator grant only for recorded in-flight submissions.
-- This receipt cannot authorize a new submission or survive a grant change.
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
INSERT INTO workflow_capability_recoveries
 SELECT r.id,r.repo_id,r.job_name,CASE WHEN r.tested_sha<>'' THEN r.tested_sha ELSE r.sha END,
 g.capability_json,json_remove(g.capability_json,'$.extended_resources'),g.granted_by,g.granted_at
 FROM runs r JOIN workflow_capability_grants g ON g.repo_id=r.repo_id
 WHERE r.status='running' AND r.job_name<>'' AND g.revoked_at IS NULL
 AND json_type(g.capability_json,'$.extended_resources')='object'
 AND json_extract(g.capability_json,'$.extended_resources')<>'{}';
CREATE TRIGGER workflow_capability_recoveries_immutable BEFORE UPDATE ON workflow_capability_recoveries
BEGIN SELECT RAISE(ABORT,'workflow capability recovery is immutable'); END;

UPDATE workflow_capability_grants
 SET capability_json=json_remove(capability_json,'$.extended_resources')
 WHERE json_type(capability_json,'$.extended_resources') IS NOT NULL;

-- Ending or replacing an attempt must not revive its receipt under the same ID.
CREATE TRIGGER workflow_capability_recovery_run_changed
AFTER UPDATE OF status,job_name,sha,tested_sha ON runs
WHEN OLD.status<>NEW.status OR OLD.job_name<>NEW.job_name
 OR OLD.sha<>NEW.sha OR OLD.tested_sha<>NEW.tested_sha
BEGIN DELETE FROM workflow_capability_recoveries WHERE run_id=OLD.id; END;
