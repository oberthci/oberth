package store

const executionRetirementMigrationVersion = 41

// Preserve history without manufacturing cleanup or transferring authority.
const executionRetirementSchema = `
CREATE TRIGGER vm_executions_retired_insert BEFORE INSERT ON vm_executions
BEGIN SELECT RAISE(ABORT, 'retired execution history is read-only'); END;
CREATE TRIGGER vm_executions_retired_update BEFORE UPDATE ON vm_executions
BEGIN SELECT RAISE(ABORT, 'retired execution history is read-only'); END;
CREATE TRIGGER vm_executions_retired_delete BEFORE DELETE ON vm_executions
BEGIN SELECT RAISE(ABORT, 'retired execution history is read-only'); END;
CREATE TRIGGER vm_capacity_slots_retired_insert BEFORE INSERT ON vm_capacity_slots
BEGIN SELECT RAISE(ABORT, 'retired execution history is read-only'); END;
CREATE TRIGGER vm_capacity_slots_retired_update BEFORE UPDATE ON vm_capacity_slots
BEGIN SELECT RAISE(ABORT, 'retired execution history is read-only'); END;
CREATE TRIGGER vm_capacity_slots_retired_delete BEFORE DELETE ON vm_capacity_slots
BEGIN SELECT RAISE(ABORT, 'retired execution history is read-only'); END;
CREATE TRIGGER kubevirt_grants_retired_insert BEFORE INSERT ON kubevirt_grants
BEGIN SELECT RAISE(ABORT, 'retired execution history is read-only'); END;
CREATE TRIGGER kubevirt_grants_retired_update BEFORE UPDATE ON kubevirt_grants
BEGIN SELECT RAISE(ABORT, 'retired execution history is read-only'); END;
CREATE TRIGGER kubevirt_grants_retired_delete BEFORE DELETE ON kubevirt_grants
BEGIN SELECT RAISE(ABORT, 'retired execution history is read-only'); END;
`
