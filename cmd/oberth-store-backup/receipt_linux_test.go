//go:build linux

package main

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"
)

// Models an elapsed absolute deadline whose asynchronous cancellation delivery
// has not run. Err intentionally remains nil and Done remains open.
type delayedDeadline struct {
	context.Context
	deadline time.Time
}

func (c *delayedDeadline) Deadline() (time.Time, bool) { return c.deadline, true }

type expireWriter struct {
	bytes.Buffer
	ctx *delayedDeadline
}

func (w *expireWriter) Write(p []byte) (int, error) {
	w.ctx.deadline = time.Now().Add(-time.Second)
	return w.Buffer.Write(p)
}

func TestReceiptRejectsDelayedCancellation(t *testing.T) {
	ctx := &delayedDeadline{context.Background(), time.Now().Add(-time.Second)}
	var out bytes.Buffer
	if err := emitReceipt(ctx, &out, map[string]bool{"passed": true}); !errors.Is(err, context.DeadlineExceeded) || out.Len() != 0 {
		t.Fatal("expired deadline emitted success")
	}
	ctx.deadline = time.Now().Add(time.Hour)
	w := &expireWriter{ctx: ctx}
	if err := emitReceipt(ctx, w, map[string]bool{"passed": true}); !errors.Is(err, context.DeadlineExceeded) || w.Len() == 0 {
		t.Fatal("write crossing deadline returned success")
	}
	ctx.deadline = time.Now().Add(time.Hour)
	if err := emitReceipt(ctx, &out, map[string]bool{"passed": true}); err != nil {
		t.Fatal(err)
	}
}
