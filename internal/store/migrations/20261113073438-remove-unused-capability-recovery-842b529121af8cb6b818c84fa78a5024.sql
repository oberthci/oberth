-- Purpose: Remove capability recovery receipts with no production issuer.
-- Rollback: Restore the verified pre-upgrade database backup.
-- Never learn historical authority from a live object or reuse an old grant hash.
DROP TRIGGER IF EXISTS workflow_capability_recovery_run_changed;
DROP TABLE workflow_capability_recoveries;
