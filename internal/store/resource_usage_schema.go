package store

const resourceUsageMigrationVersion = 36
const resourceUsageSchema = `
CREATE TABLE step_resource_usage (
 run_id TEXT NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
 repo_id INTEGER NOT NULL REFERENCES repositories(id),
 node_id TEXT NOT NULL,
 pod_uid TEXT NOT NULL,
 container_id TEXT NOT NULL,
 burn TEXT NOT NULL,
 step TEXT NOT NULL,
 last_observed_at INTEGER NOT NULL,
 observation_json TEXT NOT NULL CHECK(length(observation_json)<=8192),
 PRIMARY KEY(run_id,node_id,pod_uid,container_id)
);
CREATE INDEX step_resource_usage_repo_idx ON step_resource_usage(repo_id,last_observed_at DESC,run_id,node_id,pod_uid,container_id);
CREATE INDEX step_resource_usage_step_idx ON step_resource_usage(repo_id,burn,step,last_observed_at DESC);
`
