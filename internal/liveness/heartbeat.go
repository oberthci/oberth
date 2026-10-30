// Package liveness tracks process-local progress without doing work on the
// HTTP probe path. External dependency availability must never be a heartbeat.
package liveness

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"syscall"
	"time"
)

// MaxStall deliberately tolerates long CPU and writer queues under CI load.
const MaxStall = 5 * time.Minute
const Interval = 30 * time.Second

// BusyReadinessWindow requires confirmed contention across several writer
// probes before a readable WAL is removed from service. One busy sample must
// never mature just because another probe is slow or the watcher has stopped.
const BusyReadinessWindow = 90 * time.Second

var ErrStalled = errors.New("local progress stalled")

type sample struct {
	progressed time.Time
	suspended  bool
}

// Heartbeat's zero value is unarmed, for loops that start after recovery. Once
// armed, only real progress renews it. Reads never acquire a worker's lock.
type Heartbeat struct {
	state atomic.Pointer[sample]
	// Now is immutable after first use; nil uses the monotonic process clock.
	Now   func() time.Time
	probe atomic.Pointer[probeResult]
}

func (heartbeat *Heartbeat) clock() time.Time {
	if heartbeat.Now != nil {
		return heartbeat.Now()
	}
	return time.Now()
}

func (heartbeat *Heartbeat) Beat() {
	next := &sample{progressed: heartbeat.clock()}
	for {
		before := heartbeat.state.Load()
		if before != nil && before.suspended {
			return
		}
		if heartbeat.state.CompareAndSwap(before, next) {
			return
		}
	}
}

func (heartbeat *Heartbeat) Check() error { return heartbeat.CheckAt(heartbeat.clock()) }

// CheckAt allows several components to be evaluated at one instant. time.Time
// preserves the monotonic clock, so wall-clock corrections cannot cause wedges.
func (heartbeat *Heartbeat) CheckAt(now time.Time) error {
	state := heartbeat.state.Load()
	if state != nil && !state.suspended && now.Sub(state.progressed) > MaxStall {
		return fmt.Errorf("%w: age=%s last_writer_error=%s", ErrStalled, now.Sub(state.progressed), heartbeat.ErrorClass())
	}
	return nil
}

// Suspend owns its pause. A second caller cannot overwrite an existing pause,
// and a stale resume cannot clear a newer one. Progress from another goroutine
// cannot renew a suspended heartbeat. External handoffs must still strip it.
func (heartbeat *Heartbeat) Suspend() func() {
	mine := &sample{suspended: true}
	for {
		before := heartbeat.state.Load()
		if before != nil && before.suspended {
			return func() {}
		}
		if heartbeat.state.CompareAndSwap(before, mine) {
			break
		}
	}
	return func() { heartbeat.state.CompareAndSwap(mine, &sample{progressed: heartbeat.clock()}) }
}

type probeResult struct {
	err       error
	class     string
	observed  time.Time
	busySince time.Time
}

// ProbeError is a completed failure for readiness. Busy/locked errors must be
// confirmed across BusyReadinessWindow; other hard failures are immediate.
// Deadlines belong to liveness: a readable WAL may stay ready while its writer
// is wedged. All samples, including transient busy errors, remain diagnostic.
func (heartbeat *Heartbeat) ProbeError() error {
	result := heartbeat.probe.Load()
	if result == nil || result.class == "deadline" {
		return nil
	}
	if result.class == "busy" && result.observed.Sub(result.busySince) < BusyReadinessWindow {
		return nil
	}
	return result.err
}

// recordProbe has one writer (Watch). Only completed samples can confirm busy
// persistence. Success, deadlines and other error classes end the busy episode.
func (heartbeat *Heartbeat) recordProbe(err error) {
	result := &probeResult{err: err, class: probeErrorClass(err), observed: heartbeat.clock()}
	if result.class == "busy" {
		result.busySince = result.observed
		if previous := heartbeat.probe.Load(); previous != nil && previous.class == "busy" {
			result.busySince = previous.busySince
		}
	}
	heartbeat.probe.Store(result)
}

func (heartbeat *Heartbeat) ErrorClass() string {
	if result := heartbeat.probe.Load(); result != nil {
		return result.class
	}
	return "none"
}

func probeErrorClass(err error) string {
	switch {
	case err == nil:
		return "none"
	case errors.Is(err, context.DeadlineExceeded):
		return "deadline"
	case errors.Is(err, context.Canceled):
		return "canceled"
	case errors.Is(err, syscall.ENOSPC):
		return "disk_full"
	case errors.Is(err, syscall.EROFS):
		return "read_only"
	}
	var sqlite interface{ Code() int }
	if errors.As(err, &sqlite) {
		switch sqlite.Code() & 0xff {
		case 5, 6:
			return "busy"
		case 8:
			return "read_only"
		case 11, 26:
			return "corrupt"
		case 13:
			return "disk_full"
		}
	}
	return "database_error"
}

// Watch runs one local probe at a time. Completed non-deadline errors prove
// responsiveness, not write availability; readiness and transition logs expose
// them without causing restart loops. Deadlines never renew progress. No
// goroutine is abandoned on a timeout. The probe must honor cancellation.
func (heartbeat *Heartbeat) Watch(ctx context.Context, interval time.Duration, probe func(context.Context) error) {
	heartbeat.Beat() // one startup grace period, including the first attempt
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for ctx.Err() == nil {
		probeCtx, cancel := context.WithTimeout(ctx, MaxStall)
		err := probe(probeCtx)
		if errors.Is(probeCtx.Err(), context.DeadlineExceeded) {
			err = context.DeadlineExceeded
		}
		cancel()
		if ctx.Err() != nil {
			return
		}
		heartbeat.recordProbe(err)
		if !errors.Is(err, context.DeadlineExceeded) {
			heartbeat.Beat()
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
