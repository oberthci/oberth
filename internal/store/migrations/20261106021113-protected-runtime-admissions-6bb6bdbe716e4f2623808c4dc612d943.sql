-- Purpose: Bind protected generic runtime authority without changing host intent bytes.
-- Rollback: Restore the verified pre-upgrade database backup.
CREATE TABLE protected_runtime_admissions (
 run_id TEXT PRIMARY KEY REFERENCES vm_executions(run_id) ON DELETE RESTRICT,
 admission_json TEXT NOT NULL CHECK(json_valid(admission_json) AND length(admission_json)<=32768),
 admission_sha256 TEXT NOT NULL CHECK(length(admission_sha256)=64),
 intent_sha256 TEXT NOT NULL CHECK(length(intent_sha256)=64),
 created_at INTEGER NOT NULL
);
CREATE TRIGGER protected_runtime_admission_insert BEFORE INSERT ON protected_runtime_admissions
WHEN EXISTS(SELECT 1 FROM protected_runtime_admissions WHERE run_id IS NEW.run_id)
 OR NOT EXISTS(SELECT 1 FROM vm_executions WHERE run_id=NEW.run_id AND execution_kind='kvm-preflight'
 AND spec_identity=NEW.intent_sha256 AND submitted=0 AND cleaned=0)
BEGIN SELECT RAISE(ABORT,'protected runtime admission requires a new unsubmitted protected intent'); END;
CREATE TRIGGER protected_runtime_admission_immutable BEFORE UPDATE ON protected_runtime_admissions
BEGIN SELECT RAISE(ABORT,'protected runtime admission is immutable'); END;
CREATE TRIGGER protected_runtime_admission_no_delete BEFORE DELETE ON protected_runtime_admissions
BEGIN SELECT RAISE(ABORT,'protected runtime admission is append-only'); END;
CREATE UNIQUE INDEX protected_runtime_audit_idx ON audit_actions(resource_id)
 WHERE action='protected.runtime-admitted' AND resource_type='run';
