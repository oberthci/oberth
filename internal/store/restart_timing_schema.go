package store

const restartTimingMigrationVersion = 44

// Initial-attempt timing remains in the v30 tables. Restart attempts append
// separate evidence keyed by the immutable run.restart audit count.
const restartTimingSchema = `
CREATE TABLE IF NOT EXISTS restart_workflow_timings (
 run_id TEXT NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
 epoch INTEGER NOT NULL CHECK(epoch > 0),
 workflow_created_at INTEGER NOT NULL,
 workflow_json TEXT NOT NULL CHECK(length(workflow_json) <= 262144),
 PRIMARY KEY(run_id,epoch)
);
CREATE TABLE IF NOT EXISTS restart_step_timings (
 run_id TEXT NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
 epoch INTEGER NOT NULL CHECK(epoch > 0),
 node_id TEXT NOT NULL,
 workflow_uid TEXT NOT NULL,
 pod_uid TEXT NOT NULL,
 available INTEGER NOT NULL CHECK(available IN (0,1)),
 ordinal INTEGER NOT NULL CHECK(ordinal >= 0),
 timing_json TEXT NOT NULL CHECK(length(timing_json) <= 16384),
 PRIMARY KEY(run_id,epoch,node_id)
);
CREATE TRIGGER IF NOT EXISTS restart_workflow_timing_immutable BEFORE UPDATE ON restart_workflow_timings
BEGIN SELECT RAISE(ABORT,'restart Workflow timing is immutable'); END;
CREATE TRIGGER IF NOT EXISTS restart_step_timing_immutable BEFORE UPDATE ON restart_step_timings
WHEN NEW.run_id != OLD.run_id OR NEW.epoch != OLD.epoch OR NEW.node_id != OLD.node_id OR NEW.workflow_uid != OLD.workflow_uid
 OR (OLD.pod_uid != '' AND NEW.pod_uid != OLD.pod_uid)
 OR (OLD.available = 1 AND NEW.timing_json != OLD.timing_json)
BEGIN SELECT RAISE(ABORT,'restart step timing attempt is immutable'); END;
`
