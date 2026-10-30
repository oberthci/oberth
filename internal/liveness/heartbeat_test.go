package liveness

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

func TestHeartbeatStallThresholdAndExternalWait(t *testing.T) {
	var h Heartbeat
	start := time.Now()
	if err := h.CheckAt(start.Add(time.Hour)); err != nil {
		t.Fatal("unstarted recovery", err)
	}
	h.Now = func() time.Time { return start }
	h.Beat()
	if err := h.CheckAt(start.Add(MaxStall)); err != nil {
		t.Fatal("threshold boundary", err)
	}
	if !errors.Is(h.CheckAt(start.Add(MaxStall+time.Nanosecond)), ErrStalled) {
		t.Fatal("stalled heartbeat is live")
	}
	resume := h.Suspend()
	if err := h.CheckAt(start.Add(24 * time.Hour)); err != nil {
		t.Fatal("external wait", err)
	}
	start = start.Add(24 * time.Hour)
	resume()
	if err := h.CheckAt(start); err != nil {
		t.Fatal("resume", err)
	}
	if !errors.Is(h.CheckAt(start.Add(MaxStall+time.Second)), ErrStalled) {
		t.Fatal("resumed heartbeat never expires")
	}
}

func TestWriterHeartbeatSlowProgressStallAndRecovery(t *testing.T) {
	start := time.Now()
	var elapsed atomic.Int64
	h := Heartbeat{Now: func() time.Time { return start.Add(time.Duration(elapsed.Load())) }}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	entered := make(chan context.Context)
	complete := make(chan error)
	done := make(chan struct{})
	go func() {
		defer close(done)
		h.Watch(ctx, time.Millisecond, func(ctx context.Context) error {
			select {
			case entered <- ctx:
			case <-ctx.Done():
				return ctx.Err()
			}
			select {
			case err := <-complete:
				return err
			case <-ctx.Done():
				return ctx.Err()
			}
		})
	}()
	for attempt := 1; attempt <= 3; attempt++ {
		probeCtx := receive(t, entered)
		deadline, ok := probeCtx.Deadline()
		if !ok || time.Until(deadline) < MaxStall-time.Second {
			t.Fatal("short attempt timeout can starve a queued heartbeat")
		}
		elapsed.Store(int64(time.Duration(attempt) * 4 * time.Minute))
		// Each write takes four minutes, but real completion renews its age.
		complete <- nil
		want := start.Add(time.Duration(elapsed.Load()))
		awaitSample(t, &h, want)
		if err := h.Check(); err != nil {
			t.Fatal("slow progressing writer", err)
		}
	}
	receive(t, entered) // next write is now wedged
	elapsed.Add(int64(MaxStall))
	if err := h.Check(); err != nil {
		t.Fatal("failed before threshold", err)
	}
	elapsed.Add(int64(time.Second))
	if !errors.Is(h.Check(), ErrStalled) {
		t.Fatal("wedged writer stayed live")
	}
	complete <- context.DeadlineExceeded
	receive(t, entered) // deadlines cannot renew progress
	if !errors.Is(h.Check(), ErrStalled) {
		t.Fatal("failed write renewed progress")
	}
	complete <- nil
	awaitSample(t, &h, start.Add(time.Duration(elapsed.Load())))
	if err := h.Check(); err != nil {
		t.Fatal("writer did not recover", err)
	}
	receive(t, entered) // shutdown must cancel an in-flight write and join it
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("heartbeat did not shut down")
	}
}

func receive[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case value := <-ch:
		return value
	case <-time.After(5 * time.Second):
		t.Fatal("heartbeat did not run")
		var zero T
		return zero
	}
}

func awaitSample(t *testing.T, h *Heartbeat, want time.Time) {
	t.Helper()
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for {
		if got := h.state.Load(); got != nil && got.progressed.Equal(want) {
			return
		}
		select {
		case <-ticker.C:
		case <-deadline.C:
			t.Fatal("successful write did not publish progress")
		}
	}
}

func TestHeartbeatConcurrentProbeReads(t *testing.T) {
	var h Heartbeat
	done := make(chan struct{})
	go func() {
		defer close(done)
		for range 1000 {
			h.Beat()
			resume := h.Suspend()
			resume()
		}
	}()
	for range 1000 {
		if err := h.Check(); err != nil {
			t.Fatal(err)
		}
	}
	<-done
}

func TestHeartbeatPauseOwnership(t *testing.T) {
	var h Heartbeat
	h.Beat()
	owner := h.Suspend()
	stray := h.Suspend()
	stray()
	h.Beat() // a stray loop tick must not clear the owner's external wait
	if err := h.CheckAt(time.Now().Add(24 * time.Hour)); err != nil {
		t.Fatal("another caller cleared an owned pause", err)
	}
	owner()
	newer := h.Suspend()
	owner() // stale and repeated resumes cannot clear the newer pause
	if err := h.CheckAt(time.Now().Add(24 * time.Hour)); err != nil {
		t.Fatal("stale resume cleared a newer pause", err)
	}
	newer()
	if !errors.Is(h.CheckAt(time.Now().Add(MaxStall+time.Second)), ErrStalled) {
		t.Fatal("owner's resume did not re-arm progress")
	}
}

type sqliteProbeError int

func (err sqliteProbeError) Error() string { return "private database failure" }
func (err sqliteProbeError) Code() int     { return int(err) }

func TestWriterHeartbeatFastErrorsStayLiveAndExposeReadinessFailure(t *testing.T) {
	for _, test := range []struct {
		name  string
		err   error
		class string
	}{
		{"disk full", syscall.ENOSPC, "disk_full"},
		{"read-only filesystem", syscall.EROFS, "read_only"},
		{"SQLite busy", sqliteProbeError(5), "busy"},
		{"SQLite locked", sqliteProbeError(6), "busy"},
		{"SQLite full", sqliteProbeError(13), "disk_full"},
		{"SQLite readonly", sqliteProbeError(8), "read_only"},
		{"SQLite corrupt", sqliteProbeError(11), "corrupt"},
		{"SQLite not a database", sqliteProbeError(26), "corrupt"},
	} {
		t.Run(test.name, func(t *testing.T) {
			start := time.Now()
			var elapsed atomic.Int64
			h := Heartbeat{Now: func() time.Time { return start.Add(time.Duration(elapsed.Load())) }}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			entered := make(chan struct{})
			complete := make(chan error)
			done := make(chan struct{})
			go func() {
				defer close(done)
				h.Watch(ctx, time.Millisecond, func(ctx context.Context) error {
					select {
					case entered <- struct{}{}:
					case <-ctx.Done():
						return ctx.Err()
					}
					select {
					case err := <-complete:
						return err
					case <-ctx.Done():
						return ctx.Err()
					}
				})
			}()
			defer func() { cancel(); receive(t, done) }()
			for range 6 {
				receive(t, entered)
				elapsed.Add(int64(2 * time.Minute))
				complete <- fmt.Errorf("write heartbeat: %w", test.err)
				awaitSample(t, &h, start.Add(time.Duration(elapsed.Load())))
				if err := h.Check(); err != nil {
					t.Fatal("fast persistent DB error caused a restart", err)
				}
				if !errors.Is(h.ProbeError(), test.err) || h.ErrorClass() != test.class {
					t.Fatalf("missing failure diagnostics: %v, %s", h.ProbeError(), h.ErrorClass())
				}
			}
			receive(t, entered)
			elapsed.Add(int64(time.Second))
			complete <- nil
			awaitSample(t, &h, start.Add(time.Duration(elapsed.Load())))
			if h.ProbeError() != nil || h.ErrorClass() != "none" {
				t.Fatal("writer recovery did not clear diagnostics")
			}
		})
	}
}
