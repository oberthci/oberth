package store

const pilotRetirementMigrationVersion = 45

// The inactive pilot has no execution path. Preserve its journal for export and
// investigation, but prevent old tooling from treating it as a live authority.
// Capacity reservations are deliberately untouched: retirement is not evidence
// that an external resource was cleaned up.
const pilotRetirementSchema = `
CREATE TRIGGER vm_suite_executions_retired_insert BEFORE INSERT ON vm_suite_executions
BEGIN SELECT RAISE(ABORT, 'retired pilot history is read-only'); END;
CREATE TRIGGER vm_suite_executions_retired_update BEFORE UPDATE ON vm_suite_executions
BEGIN SELECT RAISE(ABORT, 'retired pilot history is read-only'); END;
CREATE TRIGGER vm_suite_executions_retired_delete BEFORE DELETE ON vm_suite_executions
BEGIN SELECT RAISE(ABORT, 'retired pilot history is read-only'); END;
CREATE TRIGGER vm_suite_resources_retired_insert BEFORE INSERT ON vm_suite_resources
BEGIN SELECT RAISE(ABORT, 'retired pilot history is read-only'); END;
CREATE TRIGGER vm_suite_resources_retired_update BEFORE UPDATE ON vm_suite_resources
BEGIN SELECT RAISE(ABORT, 'retired pilot history is read-only'); END;
CREATE TRIGGER vm_suite_resources_retired_delete BEFORE DELETE ON vm_suite_resources
BEGIN SELECT RAISE(ABORT, 'retired pilot history is read-only'); END;
`
