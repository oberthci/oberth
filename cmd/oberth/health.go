package main

import (
	"context"
	"fmt"
	"github.com/oberthci/oberth/internal/publicerr"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

type serverClock struct {
	Now    func() time.Time
	Ticker func(time.Duration) (<-chan time.Time, func())
}
type health struct {
	mu                   sync.RWMutex
	clock                serverClock
	sample               func(context.Context) error
	watch                func() bool
	validLeaf            func() bool
	leafExpiry           func() time.Time
	started, stopping    bool
	success, completed   time.Time
	readySignal          chan struct{}
	readyOnce            sync.Once
	sampleReady          chan struct{}
	sampleOnceReady      sync.Once
	retentionFailures    uint64
	cacheFailures        uint64
	admissionTimeouts    atomic.Uint64
	cacheNoticeDrops     atomic.Uint64
	tlsHandshakeFailures atomic.Uint64
	nativeIncomplete     func() uint64
}

func (h *health) run(ctx context.Context) error {
	ticks, stop := h.clock.Ticker(5 * time.Second)
	defer stop()
	for {
		h.sampleOnce(ctx)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticks:
		}
	}
}
func (h *health) sampleOnce(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	err := h.sample(ctx)
	h.mu.Lock()
	defer h.mu.Unlock()
	now := h.clock.Now()
	h.completed = now
	if err == nil {
		h.success = now
		if h.sampleReady != nil {
			h.sampleOnceReady.Do(func() { close(h.sampleReady) })
		}
	}
}
func (h *health) setStarted(started bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.started = started
	if started && h.readySignal != nil {
		h.readyOnce.Do(func() { close(h.readySignal) })
	}
}
func (h *health) stop() { h.mu.Lock(); h.stopping = true; h.started = false; h.mu.Unlock() }
func (h *health) ready(context.Context) error {
	h.mu.RLock()
	defer h.mu.RUnlock()
	reason := ""
	switch {
	case h.stopping:
		reason = "stopping"
	case !h.started:
		reason = "starting"
	case h.success.IsZero():
		reason = "database not sampled"
	case h.clock.Now().Sub(h.success) >= 90*time.Second:
		reason = fmt.Sprintf("database sample %.0fs old", h.clock.Now().Sub(h.success).Seconds())
	case !h.validLeaf():
		reason = "certificate invalid"
	}
	if reason != "" {
		return healthError(reason)
	}
	return nil
}
func (h *health) live(context.Context) error {
	h.mu.RLock()
	defer h.mu.RUnlock()
	if h.completed.IsZero() || h.clock.Now().Sub(h.completed) >= 5*time.Minute {
		return healthError("sampler stalled 5m")
	}
	return nil
}
func (h *health) metrics(ctx context.Context) string {
	ready := 0
	if h.ready(ctx) == nil {
		ready = 1
	}
	h.mu.RLock()
	defer h.mu.RUnlock()
	started := 0
	if h.started {
		started = 1
	}
	age := float64(-1)
	if !h.success.IsZero() {
		age = h.clock.Now().Sub(h.success).Seconds()
	}
	expiry := float64(0)
	if h.leafExpiry != nil {
		expiry = h.leafExpiry().Sub(h.clock.Now()).Seconds()
	}
	watch := ""
	if h.watch != nil {
		synced := 0
		if h.watch() {
			synced = 1
		}
		watch = fmt.Sprintf("oberth_watch_synced %d\n", synced)
	}
	native := uint64(0)
	if h.nativeIncomplete != nil {
		native = h.nativeIncomplete()
	}
	observations := fmt.Sprintf("oberth_admission_timeouts_total %d\noberth_executor_incomplete_evidence_total %d\n", h.admissionTimeouts.Load(), native) + fmt.Sprintf("oberth_git_observation_drops_total %d\n", h.cacheNoticeDrops.Load())
	return observations + fmt.Sprintf("oberth_tls_handshake_failures_total %d\n", h.tlsHandshakeFailures.Load()) + fmt.Sprintf("oberth_build_info 1\noberth_ready %d\noberth_database_sample_age_seconds %.0f\noberth_started %d\noberth_tls_leaf_expiry_seconds %.0f\noberth_event_retention_failures_total %d\noberth_git_maintenance_failures_total %d\n", ready, age, started, expiry, h.retentionFailures, h.cacheFailures) + watch
}
func (h *health) gate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.mu.RLock()
		started := h.started
		h.mu.RUnlock()
		if started {
			next.ServeHTTP(w, r)
			return
		}
		h.writeProbe(w, r)
	})
}
func healthError(reason string) error {
	remedy := serverMessages()["unready"][1]
	switch reason {
	case "starting":
		remedy = "Wait for startup to finish, then retry."
	case "stopping":
		remedy = "The server is restarting; retry shortly."
	}
	return publicerr.New(publicerr.Unavailable, reason+".", remedy, nil)
}

func (h *health) waitSample(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-h.sampleReady:
		return nil
	}
}
