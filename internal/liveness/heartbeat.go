// Package liveness tracks process-local progress without doing work on the
// HTTP probe path. External dependency availability must never be a heartbeat.
package liveness

import (
	"context"
	"errors"
	"sync/atomic"
	"time"
)

// MaxStall deliberately tolerates long CPU and writer queues under CI load.
const MaxStall = 5 * time.Minute
const Interval = 30 * time.Second

var ErrStalled = errors.New("local progress stalled")

type sample struct {
	progressed time.Time
	suspended  bool
}

// Heartbeat's zero value is unarmed, for loops that start after recovery. Once
// armed, only real progress renews it. Reads never acquire a worker's lock.
type Heartbeat struct {
	state atomic.Pointer[sample]
	now   func() time.Time // immutable after first use; nil uses the monotonic process clock
}

func (heartbeat *Heartbeat) clock() time.Time {
	if heartbeat.now != nil {
		return heartbeat.now()
	}
	return time.Now()
}

func (heartbeat *Heartbeat) Beat() {
	heartbeat.state.Store(&sample{progressed: heartbeat.clock()})
}

func (heartbeat *Heartbeat) Check() error { return heartbeat.CheckAt(heartbeat.clock()) }

// CheckAt allows several components to be evaluated at one instant. time.Time
// preserves the monotonic clock, so wall-clock corrections cannot cause wedges.
func (heartbeat *Heartbeat) CheckAt(now time.Time) error {
	state := heartbeat.state.Load()
	if state != nil && !state.suspended && now.Sub(state.progressed) > MaxStall {
		return ErrStalled
	}
	return nil
}

// Suspend excludes an external wait, then starts a fresh local progress window.
// The owner must call the returned function when that wait ends. It must not
// suspend local locks, DB work, or ordinary idle waits. Calls are not nested.
func (heartbeat *Heartbeat) Suspend() func() {
	heartbeat.state.Store(&sample{suspended: true})
	return heartbeat.Beat
}

// Watch runs one local probe at a time. Only successful completion renews the
// heartbeat; repeated deadlines do not hide a stuck writer. No goroutine is
// abandoned on a timeout. The probe must honor cancellation.
func (heartbeat *Heartbeat) Watch(ctx context.Context, interval time.Duration, probe func(context.Context) error) {
	heartbeat.Beat() // one startup grace period, including the first attempt
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for ctx.Err() == nil {
		probeCtx, cancel := context.WithTimeout(ctx, MaxStall)
		err := probe(probeCtx)
		cancel()
		if err == nil && ctx.Err() == nil {
			heartbeat.Beat()
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
