-- Purpose: remove the unused trusted Plan/Apply tables.
-- Rollback: Restore the pre-upgrade database backup.
DROP TABLE IF EXISTS trusted_applies;
DROP TABLE IF EXISTS trusted_plans;
