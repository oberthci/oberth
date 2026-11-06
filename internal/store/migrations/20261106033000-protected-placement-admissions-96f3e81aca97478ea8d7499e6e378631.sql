-- Purpose: Retain exact same-instance protected placement and executor custody.
-- Rollback: Restore the verified pre-upgrade database backup.
CREATE TABLE protected_placement_admissions (
 run_id TEXT PRIMARY KEY REFERENCES protected_runtime_admissions(run_id) ON DELETE RESTRICT,
 admission_json TEXT NOT NULL CHECK(json_valid(admission_json) AND length(admission_json)<=524288),
 admission_sha256 TEXT NOT NULL CHECK(length(admission_sha256)=64),
 runtime_sha256 TEXT NOT NULL CHECK(length(runtime_sha256)=64),
 intent_sha256 TEXT NOT NULL CHECK(length(intent_sha256)=64),
 created_at INTEGER NOT NULL
);
CREATE TRIGGER protected_placement_admission_insert BEFORE INSERT ON protected_placement_admissions
WHEN EXISTS(SELECT 1 FROM protected_placement_admissions WHERE run_id IS NEW.run_id)
 OR NOT EXISTS(SELECT 1 FROM protected_runtime_admissions g JOIN vm_executions v ON v.run_id=g.run_id
 WHERE g.run_id=NEW.run_id AND g.admission_sha256=NEW.runtime_sha256 AND g.intent_sha256=NEW.intent_sha256
 AND v.execution_kind='kvm-preflight' AND v.spec_identity=NEW.intent_sha256 AND v.submitted=0 AND v.cleaned=0)
BEGIN SELECT RAISE(ABORT,'protected placement requires a new unsubmitted generic runtime'); END;
CREATE TRIGGER protected_placement_admission_immutable BEFORE UPDATE ON protected_placement_admissions
BEGIN SELECT RAISE(ABORT,'protected placement admission is immutable'); END;
CREATE TRIGGER protected_placement_admission_no_delete BEFORE DELETE ON protected_placement_admissions
BEGIN SELECT RAISE(ABORT,'protected placement admission is append-only'); END;
CREATE UNIQUE INDEX protected_placement_audit_idx ON audit_actions(resource_id)
 WHERE action='protected.placement-admitted' AND resource_type='run';
CREATE UNIQUE INDEX protected_placement_normal_idx ON protected_placement_admissions(json_extract(admission_json,'$.placement.normal_workflow'));
CREATE UNIQUE INDEX protected_placement_preflight_idx ON protected_placement_admissions(json_extract(admission_json,'$.placement.preflight_workflow'));
CREATE TRIGGER protected_placement_phase_names BEFORE INSERT ON protected_placement_admissions
WHEN EXISTS(SELECT 1 FROM protected_placement_admissions p WHERE
 json_extract(p.admission_json,'$.placement.normal_workflow') IN (json_extract(NEW.admission_json,'$.placement.normal_workflow'),json_extract(NEW.admission_json,'$.placement.preflight_workflow'))
 OR json_extract(p.admission_json,'$.placement.preflight_workflow') IN (json_extract(NEW.admission_json,'$.placement.normal_workflow'),json_extract(NEW.admission_json,'$.placement.preflight_workflow')))
BEGIN SELECT RAISE(ABORT,'protected phase name already has an immutable owner'); END;

CREATE UNIQUE INDEX protected_placement_source_audit_idx ON audit_actions(resource_id)
 WHERE action='protected.placement-source' AND resource_type='workflow';
