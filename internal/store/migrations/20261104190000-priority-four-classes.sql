-- Purpose: Add low/standard/promotion/high execution priority with legacy input compatibility.
-- Rollback: Restore the pre-upgrade database backup; older binaries cannot interpret new classes.
-- Preserve historical requested/accepted values, actors, reasons, timestamps and audit rows.
CREATE TABLE run_priority_overrides_four (
 run_id TEXT PRIMARY KEY REFERENCES runs(id) ON DELETE CASCADE,
 priority TEXT NOT NULL CHECK(priority IN ('low','standard','promotion','high','normal','highest')),
 reason TEXT NOT NULL CHECK(length(reason) BETWEEN 1 AND 512),
 actor TEXT NOT NULL,
 updated_at INTEGER NOT NULL
) WITHOUT ROWID;
INSERT INTO run_priority_overrides_four SELECT * FROM run_priority_overrides;
DROP TABLE run_priority_overrides;
ALTER TABLE run_priority_overrides_four RENAME TO run_priority_overrides;
CREATE TABLE priority_grants_four (
 identity TEXT PRIMARY KEY,
 max_class TEXT NOT NULL CHECK(max_class IN ('promotion','high','highest')),
 expires INTEGER NOT NULL,
 granted_by TEXT NOT NULL
) WITHOUT ROWID;
INSERT INTO priority_grants_four SELECT * FROM priority_grants;
DROP TABLE priority_grants;
ALTER TABLE priority_grants_four RENAME TO priority_grants;
CREATE TABLE run_priority_requests_four (
 run_id TEXT PRIMARY KEY REFERENCES runs(id) ON DELETE CASCADE,
 requested TEXT NOT NULL,
 accepted TEXT NOT NULL CHECK(accepted IN ('low','standard','promotion','high','normal','highest')),
 reason TEXT NOT NULL,
 actor TEXT NOT NULL,
 refusal TEXT NOT NULL
) WITHOUT ROWID;
INSERT INTO run_priority_requests_four SELECT * FROM run_priority_requests;
DROP TABLE run_priority_requests;
ALTER TABLE run_priority_requests_four RENAME TO run_priority_requests;
