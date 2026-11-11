-- Purpose: Bound ref glob searches using their literal prefix.
-- Rollback: Drop runs_ref_prefix_idx and remove this migration receipt.
CREATE INDEX runs_ref_prefix_idx ON runs(ref,queue_sequence);
