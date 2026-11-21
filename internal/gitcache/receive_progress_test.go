package gitcache

import (
	"context"
	"errors"
	"io"
	"k8s.io/utils/clock"
	"testing"
	"time"
)

func receiveIdleFixture(t *testing.T) (*receiveProgress, context.Context, *bool) {
	t.Helper()
	ctx, cancel := context.WithCancelCause(t.Context())
	t.Cleanup(func() { cancel(nil) })
	native := &commandProgress{now: (clock.RealClock{}).Now, timer: &admissionIdleTimer{}, idle: time.Hour, cancel: cancel}
	t.Cleanup(native.close)
	active := true
	return &receiveProgress{progress: native, begin: func() { active = true }, end: func() { active = false }}, ctx, &active
}

func TestReceiveIdleStillBoundsClientInput(t *testing.T) {
	t.Parallel()
	p, ctx, active := receiveIdleFixture(t)
	expireReceiveIdle(p.progress)
	if !*active || !errors.Is(context.Cause(ctx), context.DeadlineExceeded) {
		t.Fatal("silent pusher escaped idle bound", context.Cause(ctx))
	}
}

func TestReceiveIdleBoundsResponseWritesDuringServerWork(t *testing.T) {
	t.Parallel()
	for _, writingFirst := range []bool{false, true} {
		p, ctx, active := receiveIdleFixture(t)
		if writingFirst {
			p.write(true)
		}
		p.process()
		if !writingFirst {
			expireReceiveIdle(p.progress)
			if ctx.Err() != nil || *active {
				t.Fatal("server work spent idle")
			}
			p.write(true)
		}
		// stdout and stderr may write concurrently. Finishing either cannot
		// disable the bound while the other still waits on the pusher.
		p.write(true)
		p.write(false)
		expireReceiveIdle(p.progress)
		if !*active || !errors.Is(context.Cause(ctx), context.DeadlineExceeded) {
			t.Fatal("blocked response escaped idle bound", context.Cause(ctx))
		}
		p.write(false)
		if *active {
			t.Fatal("finished writes left SSH idle running")
		}
	}
}

func TestReceiveWriterPausesAfterSuccessAndFailure(t *testing.T) {
	t.Parallel()
	for _, failure := range []error{nil, io.ErrClosedPipe} {
		p, ctx, active := receiveIdleFixture(t)
		p.process()
		writer := p.writer(receiveWriteFunc(func(b []byte) (int, error) {
			if !*active || p.progress.paused != 0 {
				t.Error("write was not bounded")
			}
			return len(b), failure
		}))
		if n, err := writer.Write([]byte("status")); n != 6 || !errors.Is(err, failure) {
			t.Fatal(n, err)
		}
		expireReceiveIdle(p.progress)
		if ctx.Err() != nil || *active {
			t.Fatal("completed write left idle running")
		}
	}
}

type receiveWriteFunc func([]byte) (int, error)

func (f receiveWriteFunc) Write(b []byte) (int, error) { return f(b) }

// Deliver an overdue callback without waiting for a wall-clock duration.
func expireReceiveIdle(p *commandProgress) {
	p.mu.Lock()
	p.deadline = time.Time{}
	p.mu.Unlock()
	p.expire()
}

func TestQueuedIdleCallbackHonorsFreshProgressAndResume(t *testing.T) {
	t.Parallel()
	for _, reset := range []func(*commandProgress){func(p *commandProgress) { p.touch(1) }, func(p *commandProgress) { p.pause(); p.resume() }} {
		now := time.Unix(0, 0)
		ctx, cancel := context.WithCancelCause(t.Context())
		defer cancel(nil)
		timer := &admissionIdleTimer{}
		p := &commandProgress{timer: timer, now: func() time.Time { return now }, idle: time.Minute, cancel: cancel, deadline: now}
		reset(p)
		p.expire()
		if ctx.Err() != nil {
			t.Fatal("queued callback ignored the fresh deadline")
		}
		now = now.Add(time.Minute)
		p.expire()
		if !errors.Is(context.Cause(ctx), context.DeadlineExceeded) {
			t.Fatal("fresh deadline was not enforced")
		}
	}
}
