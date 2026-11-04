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
