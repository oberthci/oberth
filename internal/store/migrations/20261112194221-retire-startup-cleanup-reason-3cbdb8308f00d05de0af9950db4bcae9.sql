-- Purpose: retire startup cleanup reason.
-- Rollback: Restore the pre-upgrade database backup.
UPDATE runs SET error='Workflow create outcome uncertain; cleanup pending' WHERE error='Resource admission expired: no scheduled pod or progress; cleanup pending';
