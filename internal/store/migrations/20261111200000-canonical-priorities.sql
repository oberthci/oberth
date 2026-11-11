-- Purpose: Store only low, standard, promotion and high priority classes.
-- Rollback: Restore the pre-upgrade database backup.
CREATE TABLE run_priority_overrides_canonical (run_id TEXT PRIMARY KEY REFERENCES runs(id) ON DELETE CASCADE, priority TEXT NOT NULL CHECK(priority IN ('low','standard','promotion','high')), reason TEXT NOT NULL CHECK(length(reason) BETWEEN 1 AND 512), actor TEXT NOT NULL, updated_at INTEGER NOT NULL) WITHOUT ROWID;
INSERT INTO run_priority_overrides_canonical (run_id,priority,reason,actor,updated_at) SELECT run_id,CASE priority WHEN 'normal' THEN 'standard' WHEN 'highest' THEN 'high' ELSE priority END,reason,actor,updated_at FROM run_priority_overrides;
DROP TABLE run_priority_overrides;
ALTER TABLE run_priority_overrides_canonical RENAME TO run_priority_overrides;
CREATE TABLE priority_grants_canonical (identity TEXT PRIMARY KEY, max_class TEXT NOT NULL CHECK(max_class IN ('promotion','high')), expires INTEGER NOT NULL, granted_by TEXT NOT NULL) WITHOUT ROWID;
INSERT INTO priority_grants_canonical (identity,max_class,expires,granted_by) SELECT identity,CASE max_class WHEN 'normal' THEN 'standard' WHEN 'highest' THEN 'high' ELSE max_class END,expires,granted_by FROM priority_grants;
DROP TABLE priority_grants;
ALTER TABLE priority_grants_canonical RENAME TO priority_grants;
CREATE TABLE run_priority_requests_canonical (run_id TEXT PRIMARY KEY REFERENCES runs(id) ON DELETE CASCADE, requested TEXT NOT NULL, accepted TEXT NOT NULL CHECK(accepted IN ('low','standard','promotion','high')), reason TEXT NOT NULL, actor TEXT NOT NULL, refusal TEXT NOT NULL) WITHOUT ROWID;
INSERT INTO run_priority_requests_canonical (run_id,requested,accepted,reason,actor,refusal) SELECT run_id,CASE requested WHEN 'normal' THEN 'standard' WHEN 'highest' THEN 'high' ELSE requested END,CASE accepted WHEN 'normal' THEN 'standard' WHEN 'highest' THEN 'high' ELSE accepted END,reason,actor,refusal FROM run_priority_requests;
DROP TABLE run_priority_requests;
ALTER TABLE run_priority_requests_canonical RENAME TO run_priority_requests;
