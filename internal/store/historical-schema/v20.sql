CREATE TABLE vm_executions_v20 (
    run_id TEXT PRIMARY KEY REFERENCES runs(id),
    execution_kind TEXT NOT NULL DEFAULT 'vm' CHECK (execution_kind IN ('vm','kvm-preflight')),
    intent_json TEXT NOT NULL,
    spec_identity TEXT NOT NULL,
    instance_json TEXT NOT NULL DEFAULT '',
    workflow_uid TEXT NOT NULL DEFAULT '',
    receipt_json TEXT NOT NULL DEFAULT '',
    refusal_json TEXT NOT NULL DEFAULT '',
    submitted INTEGER NOT NULL DEFAULT 0 CHECK (submitted IN (0,1)),
    rejected INTEGER NOT NULL DEFAULT 0 CHECK (rejected IN (0,1)),
    cleaning INTEGER NOT NULL DEFAULT 0 CHECK (cleaning IN (0,1)),
    cleaned INTEGER NOT NULL DEFAULT 0 CHECK (cleaned IN (0,1)),
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL,
    CHECK (cleaned=0 OR cleaning=1),
    CHECK (receipt_json='' OR refusal_json=''),
    CHECK (execution_kind!='vm' OR (workflow_uid='' AND receipt_json='' AND refusal_json=''
        AND (instance_json='' OR submitted=1)
        AND (rejected=0 OR (submitted=1 AND instance_json=''))
        AND (cleaned=0 OR submitted=0 OR rejected=1 OR instance_json!=''))),
    CHECK (execution_kind!='kvm-preflight' OR (instance_json=''
        AND (workflow_uid='' OR (submitted=1 AND rejected=0))
        AND (rejected=0 OR (submitted=1 AND workflow_uid=''))
        AND (receipt_json='' OR (submitted=1 AND rejected=0 AND workflow_uid!=''))
        AND (cleaned=0 OR submitted=0 OR rejected=1 OR workflow_uid!='')))
);
INSERT INTO vm_executions_v20(run_id,intent_json,spec_identity,instance_json,submitted,rejected,cleaning,cleaned,created_at,updated_at)
SELECT run_id,intent_json,spec_identity,instance_json,submitted,rejected,cleaning,cleaned,created_at,updated_at FROM vm_executions;
DROP TABLE vm_executions;
ALTER TABLE vm_executions_v20 RENAME TO vm_executions;
CREATE UNIQUE INDEX vm_execution_capacity ON vm_executions(cleaned) WHERE cleaned=0;

CREATE TRIGGER vm_executions_no_delete
BEFORE DELETE ON vm_executions
BEGIN
    SELECT RAISE(ABORT, 'VM executions are append-only');
END;

-- REPLACE otherwise bypasses DELETE triggers when recursive_triggers is off.
-- Its NULL cleaned value also becomes the column default after BEFORE INSERT.
CREATE TRIGGER vm_executions_no_replace
BEFORE INSERT ON vm_executions
WHEN EXISTS (SELECT 1 FROM vm_executions WHERE run_id IS NEW.run_id)
    OR (COALESCE(NEW.cleaned, 0) = 0 AND EXISTS (SELECT 1 FROM vm_executions WHERE cleaned = 0))
BEGIN
    SELECT RAISE(ABORT, 'VM executions are append-only');
END;

CREATE TRIGGER vm_executions_guard_update
BEFORE UPDATE ON vm_executions
WHEN NOT (
    NEW.run_id IS OLD.run_id
    AND NEW.execution_kind IS OLD.execution_kind
    AND NEW.intent_json IS OLD.intent_json
    AND NEW.spec_identity IS OLD.spec_identity
    AND NEW.created_at IS OLD.created_at
    AND (OLD.workflow_uid = '' OR NEW.workflow_uid IS OLD.workflow_uid)
    AND (OLD.receipt_json = '' OR NEW.receipt_json IS OLD.receipt_json)
    AND (OLD.refusal_json = '' OR NEW.refusal_json IS OLD.refusal_json)
    AND COALESCE(NEW.cleaned, 0) >= OLD.cleaned
)
BEGIN
    SELECT RAISE(ABORT, 'VM execution evidence is immutable');
END;