package app

import (
	"context"
	"errors"
	"sync"
	"time"
)

const readinessMaxAge = 15 * time.Second

var (
	errReadinessStale         = errors.New("app: readiness probe pending or stale")
	errReadinessDependencies  = errors.New("app: health dependencies are unavailable")
	errReadinessDatabase      = errors.New("app: database is unavailable")
	errReadinessNoUpstream    = errors.New("app: no upstream is configured")
	errReadinessConfiguration = errors.New("app: upstream configuration is unavailable")
)

// ReadinessSnapshot prevents database/credential checks from blocking HTTP
// readiness requests. An absent or stale sample fails closed.
type ReadinessSnapshot struct {
	mu      sync.RWMutex
	checked time.Time
	err     error
	// Transition state belongs to the shared snapshot, like the sample itself.
	observed     bool
	wasReady     bool
	unreadySince time.Time
}

func (snapshot *ReadinessSnapshot) ready() error {
	snapshot.mu.RLock()
	defer snapshot.mu.RUnlock()
	return snapshot.readyAt(time.Now())
}

// readyAt requires snapshot.mu to be held.
func (snapshot *ReadinessSnapshot) readyAt(now time.Time) error {
	if snapshot.checked.IsZero() || now.Sub(snapshot.checked) > readinessMaxAge {
		return errReadinessStale
	}
	return snapshot.err
}

func (health Health) refreshReadiness(ctx context.Context) {
	// ReadyGate is process-local; include startup recovery in transition logs.
	var gateErr error
	if health.ReadyGate != nil {
		gateErr = health.ReadyGate(ctx)
	}
	started := time.Now()
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	dbWait, err := health.probeReadySample(ctx)
	now := time.Now()
	snapshot := health.Readiness
	snapshot.mu.Lock()
	// A deadline can mean waiting for the single shared DB connection. Retain
	// the last green sample without renewing its age, even if already stale.
	// Cancellation and all hard errors still replace it immediately.
	retainGreen := errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, context.Canceled) &&
		!errors.Is(ctx.Err(), context.Canceled) && !snapshot.checked.IsZero() && snapshot.err == nil
	if !retainGreen {
		snapshot.checked, snapshot.err = now, err
	}
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
	sampleAge := now.Sub(snapshot.checked)
	snapshot.observed, snapshot.wasReady = true, isReady
	snapshot.mu.Unlock()
	if changed && health.Log != nil {
		state := "unready"
		if isReady {
			state = "ready"
		}
		health.Log("readiness: state=%s reason=%s sample_age=%s db_wait=%s probe_duration=%s outage_duration=%s",
			state, reason, sampleAge, dbWait, now.Sub(started), outage)
	}
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

// WatchReadiness probes immediately, then on a two-second ticker. Probes run
// serially and can take up to three seconds, so slow probes reduce the cadence.
// All remote probes must honor the context to avoid stalling process shutdown.
func (health Health) WatchReadiness(ctx context.Context) {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		health.refreshReadiness(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
