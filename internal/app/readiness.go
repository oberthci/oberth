package app

import (
	"context"
	"errors"
	"sync"
	"time"
)

// ReadinessSnapshot prevents database/credential checks from blocking HTTP
// readiness requests. An absent or stale sample fails closed.
type ReadinessSnapshot struct {
	mu      sync.RWMutex
	checked time.Time
	err     error
}

func (snapshot *ReadinessSnapshot) ready() error {
	snapshot.mu.RLock()
	defer snapshot.mu.RUnlock()
	if snapshot.checked.IsZero() || time.Since(snapshot.checked) > 15*time.Second {
		return errors.New("app: readiness probe pending or stale")
	}
	return snapshot.err
}

func (health Health) refreshReadiness(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	err := health.probeReady(ctx)
	health.Readiness.mu.Lock()
	health.Readiness.checked, health.Readiness.err = time.Now(), err
	health.Readiness.mu.Unlock()
}

// WatchReadiness probes immediately, then every two seconds. All remote probes
// must honor the context so a stalled dependency cannot stall process shutdown.
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
