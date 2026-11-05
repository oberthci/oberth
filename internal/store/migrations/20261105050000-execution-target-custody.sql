-- Purpose: Retain immutable canonical target and pre-Workflow resource custody.
-- Rollback: Restore a compatible verified pre-upgrade backup.
CREATE TABLE execution_target_bindings (
 run_id TEXT PRIMARY KEY REFERENCES runs(id) ON DELETE CASCADE,
 workflow_name TEXT NOT NULL UNIQUE,
 binding_json TEXT NOT NULL CHECK(json_valid(binding_json) AND length(binding_json)<=2097152),
 cancelled INTEGER NOT NULL DEFAULT 0 CHECK(cancelled IN (0,1)),
 admission_json TEXT NOT NULL DEFAULT '',
 cleanup_receipt TEXT NOT NULL DEFAULT ''
);
CREATE TRIGGER execution_target_binding_immutable BEFORE UPDATE OF run_id,workflow_name,binding_json ON execution_target_bindings BEGIN
 SELECT RAISE(ABORT,'immutable execution target binding');
END;
CREATE TRIGGER execution_target_cancel_monotonic BEFORE UPDATE OF cancelled ON execution_target_bindings WHEN OLD.cancelled=1 AND NEW.cancelled!=1 BEGIN
 SELECT RAISE(ABORT,'execution target cancellation cannot be revoked');
END;
CREATE TRIGGER execution_target_admission_immutable BEFORE UPDATE OF admission_json ON execution_target_bindings WHEN OLD.admission_json!='' AND OLD.admission_json!=NEW.admission_json BEGIN
 SELECT RAISE(ABORT,'immutable execution target admission');
END;
CREATE TRIGGER execution_target_cleanup_immutable BEFORE UPDATE OF cleanup_receipt ON execution_target_bindings WHEN OLD.cleanup_receipt!='' AND OLD.cleanup_receipt!=NEW.cleanup_receipt BEGIN
 SELECT RAISE(ABORT,'immutable execution target cleanup receipt');
END;
CREATE TABLE execution_target_resources (
 run_id TEXT NOT NULL REFERENCES execution_target_bindings(run_id) ON DELETE RESTRICT,
 kind TEXT NOT NULL CHECK(kind IN ('Workflow','Pod','PersistentVolumeClaim','VirtualMachineInstance')),
 namespace TEXT NOT NULL,
 name TEXT NOT NULL,
 uid TEXT NOT NULL DEFAULT '',
 object_json TEXT NOT NULL CHECK(json_valid(object_json) AND length(object_json)<=4194304),
 not_before INTEGER NOT NULL,
 PRIMARY KEY(run_id,kind,namespace,name)
);
CREATE TRIGGER execution_target_resource_immutable BEFORE UPDATE OF run_id,kind,namespace,name,object_json,not_before ON execution_target_resources BEGIN
 SELECT RAISE(ABORT,'immutable execution target create intent');
END;
CREATE TRIGGER execution_target_uid_immutable BEFORE UPDATE OF uid ON execution_target_resources WHEN OLD.uid!='' AND OLD.uid!=NEW.uid BEGIN
 SELECT RAISE(ABORT,'immutable execution target object UID');
END;
