package store

const executionRetirementMigrationVersion = 47

// Stage 2 retirement is deferred. No live journal, capacity slot, grant or
// cancellation is removed or frozen. A Recreate upgrade must reach owner
// recovery even with work in flight; checking for an empty database here would
// strand the very owner that can observe cleanup. Runtime recovery retains the
// interlocks and finishes cleanup before admitting replacement work.
const executionRetirementSchema = `SELECT 1;`
