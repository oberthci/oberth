package store

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/oberthci/oberth/internal/model"
)

const executionRetirementMigrationVersion = 47

// Stage 2 retirement is deferred. Keep the legacy execution journal, capacity
// interlocks and grants writable until operators migrate their pipelines.
// The migration runs before owner recovery. A refusal leaves obligations intact
// for the previous binary to settle; subsequent starts retain normal recovery.
func guardExecutionUpgrade(ctx context.Context, tx *sql.Tx, _ *model.AuditHead, _ ProtocolConfig) error {
	var executions, slots, running, cleanup int
	if err := tx.QueryRowContext(ctx, `SELECT
 (SELECT COUNT(*) FROM vm_executions WHERE cleaned=0),
 (SELECT COUNT(*) FROM vm_capacity_slots),
 (SELECT COUNT(*) FROM runs WHERE status='running'),
 (SELECT COUNT(*) FROM run_cancellations WHERE completed_at IS NULL)`).Scan(&executions, &slots, &running, &cleanup); err != nil {
		return fmt.Errorf("check staged execution upgrade: %w", err)
	}
	// Resource-template VM Workflows need not have a vm_executions row. Refuse
	// every running run rather than guess its workload from a mutable checkout.
	if executions+slots+running+cleanup != 0 {
		return fmt.Errorf("%w: drain legacy VM work before schema 47: %d uncleaned executions, %d capacity slots, %d running runs, %d pending cleanup obligations; use the previous binary to finish cleanup, then retry", ErrInvalidState, executions, slots, running, cleanup)
	}
	return nil
}
