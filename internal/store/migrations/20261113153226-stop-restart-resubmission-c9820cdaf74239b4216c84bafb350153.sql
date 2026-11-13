-- Purpose: Stop automatic resubmission after an owner restart.
-- Rollback: Restore the pre-upgrade database backup.
-- A reset already queued by the previous version is not fresh intake. Restore
-- its old Workflow name, when recorded, for read-only adoption. Without one,
-- owner recovery can fail it without ever submitting a replacement Workflow.
UPDATE runs
SET status = 'running', phase = 'waiting', reason = '',
    job_name = COALESCE(NULLIF(job_name, ''),
        (SELECT NULLIF(job_name, '') FROM run_cancellations
         WHERE run_id = runs.id AND reason = 'owner_restart'), '')
WHERE status = 'queued'
  AND (reason = 're-queued after restart' OR reason LIKE 're-queued after restart; %');

-- Retire automatic restart deletion, while preserving the same marker's live
-- uncertain-POST and explicit cancel/supersede cleanup obligations.
UPDATE run_cancellations
SET completed_at = COALESCE((SELECT updated_at FROM runs WHERE id = run_id), created_at)
WHERE completed_at IS NULL AND reason = 'owner_restart' AND superseded_by = ''
  AND EXISTS (SELECT 1 FROM runs
      WHERE id = run_id AND status = 'running'
        AND phase != 'infrastructure-pending'
        AND error NOT IN ('Workflow create outcome uncertain; cleanup pending',
                          'Workflow spec mismatch; cleanup pending',
                          'Executor identity readiness deadline exceeded after one issuance retry; cleanup pending'));
