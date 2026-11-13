package store

import "slices"

const WorkflowGrantScopeMigrationID = "20261113193519-scope-secret-approvals-to-workflow-4e48838f64a9b32ddba9d914280daa25"

// AppliedStartupMigration reports a committed upgrade applied by this open
// to an existing database. Genesis and receipts from previous starts do not count.
func (s *Store) AppliedStartupMigration(id string) bool {
	return slices.Contains(s.startupMigrations, id)
}
