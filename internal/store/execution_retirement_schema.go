package store

const executionRetirementMigrationVersion = 47

// Stage 2 retirement is deferred. Keep the legacy execution journal, capacity
// interlocks and grants writable until operators migrate their pipelines.
const executionRetirementSchema = `SELECT 1;`
