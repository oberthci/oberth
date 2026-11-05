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
