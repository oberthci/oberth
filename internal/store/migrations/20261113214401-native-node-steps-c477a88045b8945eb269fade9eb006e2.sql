-- Purpose: Store Argo status nodes by node ID; remove marker-derived log offsets.
-- Rollback: Restore the verified pre-upgrade database backup.
CREATE TABLE step_results_nodes (
    run_id TEXT NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
    node_id TEXT NOT NULL CHECK (length(node_id) > 0),
    display_name TEXT NOT NULL DEFAULT '',
    template TEXT NOT NULL DEFAULT '',
    replay_evidence_complete INTEGER NOT NULL DEFAULT 0 CHECK(replay_evidence_complete IN (0,1)),
    credentialed INTEGER CHECK(credentialed IS NULL OR credentialed IN (0,1)),
    grant_evidence_json TEXT,
    node_type TEXT NOT NULL DEFAULT '',
    boundary_id TEXT NOT NULL DEFAULT '',
    phase TEXT NOT NULL DEFAULT '',
    message TEXT NOT NULL DEFAULT '',
    pod_name TEXT NOT NULL DEFAULT '',
    burn TEXT NOT NULL DEFAULT '',
    step TEXT NOT NULL DEFAULT '',
    ordinal INTEGER NOT NULL CHECK (ordinal >= 0),
    status TEXT NOT NULL CHECK (status IN ('queued','running','passed','failed','skipped','timed_out')),
    exit_code INTEGER NOT NULL,
    started_at INTEGER,
    finished_at INTEGER,
    recorded_at INTEGER NOT NULL,
    termination_json TEXT CHECK (termination_json IS NULL OR length(termination_json) <= 4096),
    PRIMARY KEY (run_id, node_id)
);
-- Existing stored rows remain readable; their original node identity was not
-- recorded. Give each retained row an opaque database identity without replay
-- or log parsing. Do not synthesize native phases, messages or template names.
INSERT INTO step_results_nodes(run_id,node_id,display_name,burn,step,ordinal,status,exit_code,started_at,finished_at,recorded_at,termination_json)
SELECT run_id,'stored:' || rowid,step,burn,step,ordinal,status,exit_code,started_at,finished_at,recorded_at,termination_json
FROM step_results;
DROP TABLE step_results;
ALTER TABLE step_results_nodes RENAME TO step_results;
CREATE INDEX step_results_order_idx ON step_results(run_id,ordinal,node_id);
