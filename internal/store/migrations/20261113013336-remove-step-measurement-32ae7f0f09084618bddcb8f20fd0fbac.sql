-- Purpose: Remove per-step measurement history and counters; retain outcomes, logs and diagnostics.
-- Rollback: Restore the verified pre-upgrade database backup; removed usage history cannot be recovered otherwise.
DROP TABLE resource_attempts;
DROP TABLE resource_attempt_inventories;
DROP TABLE pod_resource_dispositions;
DROP TABLE step_resource_usage;
ALTER TABLE step_results DROP COLUMN max_rss_bytes;
ALTER TABLE step_results DROP COLUMN memory_peak_bytes;
ALTER TABLE step_results DROP COLUMN user_cpu_ns;
ALTER TABLE step_results DROP COLUMN system_cpu_ns;
ALTER TABLE step_results DROP COLUMN declared_size;
