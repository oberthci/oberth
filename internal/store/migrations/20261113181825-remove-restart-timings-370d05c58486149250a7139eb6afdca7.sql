-- Purpose: Retire restart timing epochs while retaining current durable timing.
-- Rollback: Restore the pre-upgrade database backup.
-- Flatten the last current attempt once. No runtime reader or writer selects
-- attempts by audit counts after this migration. Missing current observations
-- stay missing rather than inheriting an earlier Workflow's identity.
CREATE TEMP TABLE retiring_restart_timings AS
SELECT t.*, a.epoch FROM run_timings t
JOIN (SELECT resource_id, count(*) AS epoch FROM audit_actions
      WHERE resource_type='run' AND action='run.restart' GROUP BY resource_id) a
ON a.resource_id=t.run_id;

UPDATE retiring_restart_timings
SET workflow_created_at=(SELECT w.workflow_created_at FROM restart_workflow_timings w
                         WHERE w.run_id=retiring_restart_timings.run_id AND w.epoch=retiring_restart_timings.epoch),
    workflow_json=(SELECT w.workflow_json FROM restart_workflow_timings w
                   WHERE w.run_id=retiring_restart_timings.run_id AND w.epoch=retiring_restart_timings.epoch);

DELETE FROM step_timings WHERE run_id IN (SELECT run_id FROM retiring_restart_timings);
INSERT INTO step_timings(run_id,node_id,workflow_uid,pod_uid,available,ordinal,timing_json)
SELECT s.run_id,s.node_id,s.workflow_uid,s.pod_uid,s.available,s.ordinal,s.timing_json
FROM restart_step_timings s JOIN retiring_restart_timings t ON s.run_id=t.run_id AND s.epoch=t.epoch
WHERE s.workflow_uid=json_extract(t.workflow_json,'$.uid');

-- Replace immutable rows, preserving milestones and admission episodes.
DELETE FROM run_timings WHERE run_id IN (SELECT run_id FROM retiring_restart_timings);
INSERT INTO run_timings(run_id,version,claimed_at,checkout_ready_at,source_seed_started_at,source_ready_at,workflow_created_at,terminal_at,workflow_json)
SELECT run_id,version,claimed_at,checkout_ready_at,source_seed_started_at,source_ready_at,workflow_created_at,terminal_at,workflow_json
FROM retiring_restart_timings;
DROP TABLE retiring_restart_timings;
DROP TABLE restart_step_timings;
DROP TABLE restart_workflow_timings;
