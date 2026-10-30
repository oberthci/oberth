package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/oberthci/oberth/internal/store"
)

const (
	readinessInterval = 5 * time.Second
	// ReadinessProbeBudget also sets the minimum SQLite readiness busy timeout.
	ReadinessProbeBudget = 30 * time.Second
	// This is an elapsed-time bound, independent of how many attempts complete.
	// The ticker drops missed ticks: full-budget reads run about every thirty seconds.
	readinessMaxAge = 90 * time.Second
)

var (
	errReadinessStale         = errors.New("app: readiness probe pending or stale")
	errReadinessDependencies  = errors.New("app: health dependencies are unavailable")
	errReadinessWriter        = errors.New("app: writer persistence window exceeded")
	errReadinessDatabase      = errors.New("app: database is unavailable")
	errReadinessNoUpstream    = errors.New("app: no upstream is configured")
	errReadinessConfiguration = errors.New("app: upstream configuration is unavailable")
)

// ReadinessSnapshot prevents database/credential checks from blocking HTTP
// readiness requests. An absent or stale sample fails closed.
type ReadinessSnapshot struct {
	mu                sync.RWMutex
	checked           time.Time
	err               error
	dbWait            time.Duration
	probeDuration     time.Duration
	retainedDeadlines uint64
	retainedBusy      uint64
	retainedLogged    bool
	// Shared by HTTP observations and the watcher so an expiry during a stalled
	// read is logged once, even if many concurrent requests observe it.
	observed     bool
	wasReady     bool
	unreadySince time.Time
}

// readyAt requires snapshot.mu to be held.
func (snapshot *ReadinessSnapshot) readyAt(now time.Time) error {
	if snapshot.checked.IsZero() || now.Sub(snapshot.checked) > readinessMaxAge {
		return errReadinessStale
	}
	return snapshot.err
}

// sampleAge requires snapshot.mu to be held.
func (snapshot *ReadinessSnapshot) sampleAge(now time.Time) time.Duration {
	if snapshot.checked.IsZero() {
		return 0
	}
	return max(time.Duration(0), now.Sub(snapshot.checked))
}

// WriteMetrics reports fixed, unlabeled process metrics without probing storage.
// Sample presence distinguishes an uninitialized snapshot from an age of zero.
func (snapshot *ReadinessSnapshot) WriteMetrics(w io.Writer) {
	snapshot.mu.RLock()
	age, retained, busy, present := snapshot.sampleAge(time.Now()).Seconds(), snapshot.retainedDeadlines, snapshot.retainedBusy, 0
	if !snapshot.checked.IsZero() {
		present = 1
	}
	snapshot.mu.RUnlock()
	_, _ = fmt.Fprintf(w, "# TYPE oberth_readiness_sample_present gauge\noberth_readiness_sample_present %d\n# TYPE oberth_readiness_sample_age_seconds gauge\noberth_readiness_sample_age_seconds %g\n# TYPE oberth_readiness_retained_deadlines_total counter\noberth_readiness_retained_deadlines_total %d\n# TYPE oberth_readiness_retained_busy_total counter\noberth_readiness_retained_busy_total %d\n", present, age, retained, busy)
}

func (health Health) observeReadiness(gateErr error) error {
	snapshot := health.Readiness
	snapshot.mu.Lock()
	now := time.Now()
	readyErr := snapshot.readyAt(now)
	reason := readinessReason(readyErr)
	if gateErr != nil {
		readyErr, reason = gateErr, "startup_recovery"
	}
	isReady := readyErr == nil
	changed := snapshot.observed && snapshot.wasReady != isReady
	if !isReady && (!snapshot.observed || snapshot.wasReady) {
		snapshot.unreadySince = now
	}
	outage := time.Duration(0)
	if isReady && !snapshot.unreadySince.IsZero() {
		outage = now.Sub(snapshot.unreadySince)
		snapshot.unreadySince = time.Time{}
	}
	age, dbWait, duration := snapshot.sampleAge(now), snapshot.dbWait, snapshot.probeDuration
	snapshot.observed, snapshot.wasReady = true, isReady
	snapshot.mu.Unlock()
	if changed && health.Log != nil {
		state := "unready"
		if isReady {
			state = "ready"
		}
		health.Log("readiness: state=%s reason=%s sample_age=%s db_wait=%s probe_duration=%s outage_duration=%s", state, reason, age, dbWait, duration, outage)
	}
	return readyErr
}

func (health Health) refreshReadiness(ctx context.Context) {
	var gateErr error
	if health.ReadyGate != nil {
		gateErr = health.ReadyGate(ctx)
	}
	// Observe before replacing the sample, including expiry during a long pause.
	health.Readiness.mu.RLock()
	initialized := !health.Readiness.checked.IsZero()
	health.Readiness.mu.RUnlock()
	if initialized {
		_ = health.observeReadiness(gateErr)
	}
	started := time.Now()
	ctx, cancel := context.WithTimeout(ctx, ReadinessProbeBudget)
	defer cancel()
	dbWait, err := health.probeReadySample(ctx)
	now := time.Now()
	snapshot := health.Readiness
	snapshot.mu.Lock()
	// Deadlines and typed SQLite contention retain the last success without
	// renewing its age. Cancellation
	// and hard errors still replace it immediately.
	retainGreen := !errors.Is(err, errReadinessWriter) && (errors.Is(err, context.DeadlineExceeded) || store.IsTransientLockError(err)) && !errors.Is(err, context.Canceled) &&
		!errors.Is(ctx.Err(), context.Canceled) && !snapshot.checked.IsZero() && snapshot.err == nil
	logRetained := false
	if retainGreen {
		if store.IsTransientLockError(err) {
			snapshot.retainedBusy++
		} else {
			snapshot.retainedDeadlines++
		}
		logRetained = !snapshot.retainedLogged
		snapshot.retainedLogged = true
	} else {
		snapshot.checked, snapshot.err = now, err
		if err == nil {
			snapshot.retainedLogged = false
		}
	}
	snapshot.dbWait, snapshot.probeDuration = dbWait, now.Sub(started)
	age := snapshot.sampleAge(now)
	snapshot.mu.Unlock()
	if logRetained && health.Log != nil {
		event := "retained_deadline"
		if store.IsTransientLockError(err) {
			event = "retained_busy"
		}
		health.Log("readiness: event=%s sample_age=%s db_wait=%s probe_duration=%s", event, age, dbWait, now.Sub(started))
	}
	_ = health.observeReadiness(gateErr)
}

func readinessReason(err error) string {
	switch {
	case err == nil:
		return "ready"
	case errors.Is(err, errReadinessStale):
		return "stale"
	case errors.Is(err, context.Canceled):
		return "canceled"
	case errors.Is(err, context.DeadlineExceeded):
		return "deadline"
	case errors.Is(err, errReadinessDependencies):
		return "dependencies"
	case errors.Is(err, errReadinessDatabase):
		return "database_error"
	case errors.Is(err, errReadinessNoUpstream):
		return "no_upstream"
	case errors.Is(err, errReadinessConfiguration):
		return "configuration_error"
	default:
		return "unknown"
	}
}

// WatchReadiness probes immediately, then on a five-second ticker. Probes run
// serially and can take up to thirty seconds, so slow probes reduce the cadence.
// All remote probes must honor the context to avoid stalling process shutdown.
func (health Health) WatchReadiness(ctx context.Context) {
	ticker := time.NewTicker(readinessInterval)
	defer ticker.Stop()
	for {
		if ctx.Err() != nil {
			return
		}
		health.refreshReadiness(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
