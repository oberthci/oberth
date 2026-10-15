//go:build linux

package main

import (
	"context"
	"encoding/json"
	"io"
	"time"
)

// Err alone can lag the absolute deadline while cancellation is being scheduled.
func checkDeadline(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if deadline, ok := ctx.Deadline(); ok && !time.Now().Before(deadline) {
		return context.DeadlineExceeded
	}
	return nil
}

// A receipt is evidence only together with successful command exit. The retained
// outer deadline still bounds blocking filesystem/output calls; this check does
// not claim that a byte stream can be recalled if its final Write blocks.
func emitReceipt(ctx context.Context, out io.Writer, receipt any) error {
	if err := checkDeadline(ctx); err != nil {
		return err
	}
	if err := json.NewEncoder(out).Encode(receipt); err != nil {
		return err
	}
	return checkDeadline(ctx)
}
