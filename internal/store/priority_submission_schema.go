package store

const prioritySubmissionMigrationVersion = 39
const prioritySubmissionSchema = `
CREATE TABLE IF NOT EXISTS priority_grants (
 identity TEXT PRIMARY KEY,
 max_class TEXT NOT NULL CHECK(max_class IN ('high','highest')),
 expires INTEGER NOT NULL,
 granted_by TEXT NOT NULL
) WITHOUT ROWID;
CREATE TABLE IF NOT EXISTS run_priority_requests (
 run_id TEXT PRIMARY KEY REFERENCES runs(id) ON DELETE CASCADE,
 requested TEXT NOT NULL,
 accepted TEXT NOT NULL CHECK(accepted IN ('normal','high','highest')),
 reason TEXT NOT NULL,
 actor TEXT NOT NULL,
 refusal TEXT NOT NULL
) WITHOUT ROWID;
INSERT OR IGNORE INTO run_priority_requests(run_id,requested,accepted,reason,actor,refusal)
 SELECT id,'',` + basePrioritySQL + `,` + priorityReasonSQL + `,actor,'' FROM runs
 WHERE status IN ('queued','running') AND (` + basePrioritySQL + `) IN ('high','highest');`
