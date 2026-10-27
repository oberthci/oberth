package store

const stepUsageMigrationVersion = 33

// No foreign keys point at step_results. Rebuild just this table in the normal
// migration transaction, preserving its key, ordering index and diagnostics.
// A legacy zero is ambiguous because every old counter defaulted to zero,
// independently. Preserve positive counters; do not infer measurements for the
// remaining zero counters, even when another counter in the row is positive.
const stepUsageSchema = `
CREATE TABLE step_results_usage (
    run_id TEXT NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
    burn TEXT NOT NULL,
    step TEXT NOT NULL,
    ordinal INTEGER NOT NULL,
    status TEXT NOT NULL CHECK (status IN ('passed', 'failed', 'skipped', 'timed_out')),
    exit_code INTEGER NOT NULL,
    log_start INTEGER NOT NULL CHECK (log_start >= 0),
    log_end INTEGER NOT NULL CHECK (log_end >= log_start),
    started_at INTEGER,
    finished_at INTEGER,
    recorded_at INTEGER NOT NULL,
    declared_size TEXT NOT NULL DEFAULT 'M' CHECK (declared_size IN ('S', 'M', 'L', 'XL')),
    max_rss_bytes INTEGER CHECK (max_rss_bytes >= 0),
    user_cpu_ns INTEGER CHECK (user_cpu_ns >= 0),
    system_cpu_ns INTEGER CHECK (system_cpu_ns >= 0),
    termination_json TEXT CHECK(termination_json IS NULL OR length(termination_json) <= 4096),
    memory_peak_bytes INTEGER CHECK (memory_peak_bytes >= 0),
    PRIMARY KEY (run_id, burn, step)
);
INSERT INTO step_results_usage
SELECT run_id,burn,step,ordinal,status,exit_code,log_start,log_end,
       started_at,finished_at,recorded_at,declared_size,
       NULLIF(max_rss_bytes,0),
       NULLIF(user_cpu_ns,0),
       NULLIF(system_cpu_ns,0),
       termination_json,NULL
FROM step_results;
DROP TABLE step_results;
ALTER TABLE step_results_usage RENAME TO step_results;
CREATE INDEX step_results_order_idx ON step_results(run_id, ordinal, burn, step);
`
