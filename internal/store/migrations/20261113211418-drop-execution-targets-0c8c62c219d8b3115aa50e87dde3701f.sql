-- Purpose: Remove execution targets and their custody journal.
-- Rollback: Restore the verified pre-upgrade database backup.
-- Only an explicit local binding may continue through the single-cluster engine.
-- Retire remote work before discarding its routing authority. Keep workflow
-- names as historical evidence, but complete cancellation intent without making
-- any local-cluster call. Old remote objects require operator cleanup in their
-- original kubeconfig context.
CREATE TEMP TABLE retired_target_runs (run_id TEXT PRIMARY KEY);
INSERT INTO retired_target_runs
 SELECT run_id FROM execution_target_bindings
 WHERE json_extract(binding_json,'$.Target') IS NOT 'local';

UPDATE runs SET status='failed', phase='infrastructure',
 error='Execution targets were removed; recovery refused. Inspect the original cluster using its kubeconfig context and start a fresh run.',
 finished_at=coalesce(finished_at,unixepoch('subsec')*1000000000),
 updated_at=unixepoch('subsec')*1000000000
 WHERE id IN (SELECT run_id FROM retired_target_runs)
 AND (status IN ('queued','running') OR phase='infrastructure-pending'
      OR error LIKE 'Workflow create outcome uncertain; cleanup pending%'
      OR error IN ('Workflow spec mismatch; cleanup pending','Executor identity readiness deadline exceeded after one issuance retry; cleanup pending'));

UPDATE run_cancellations SET completed_at=unixepoch('subsec')*1000000000
 WHERE run_id IN (SELECT run_id FROM retired_target_runs) AND completed_at IS NULL;

DROP TABLE execution_target_resources;
DROP TABLE execution_target_bindings;
DROP TABLE retired_target_runs;
