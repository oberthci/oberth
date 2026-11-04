-- Purpose: Activate immutable migration IDs without changing historical receipts.
-- Rollback: Restore the pre-upgrade database backup.
-- The runner adds/backfills the ID column and unique index in this transaction
-- before executing the first timestamped migration and recording its ID.
SELECT 1;
