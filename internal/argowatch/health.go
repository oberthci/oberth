package argowatch

import (
	"context"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/client-go/tools/cache"
)

const healthGrace = 5 * time.Second

type resourceKind int

const (
	workflowKind resourceKind = iota
	podKind
)

type attemptHealth struct {
	lastSuccess  time.Time
	failureSince time.Time
}

func (h *Hub) recordAttempt(kind resourceKind, err error) {
	h.healthMu.Lock()
	defer h.healthMu.Unlock()
	// Observe transitions before and after this attempt, preserving one
	// timestamp while outages overlap across the two resource kinds.
	h.healthSinceLocked()
	state := &h.attempts[kind]
	if err == nil {
		state.lastSuccess = h.now()
		state.failureSince = time.Time{}
	} else if state.failureSince.IsZero() {
		state.failureSince = h.now()
	}
	h.healthSinceLocked()
}

// Healthy reports whether both caches are synced and their API observations
// remain available. An idle open watch does not expire merely for being idle.
func (h *Hub) Healthy() bool {
	_, unhealthy := h.UnhealthySince()
	return !unhealthy
}

// UnhealthySince returns the start of the outstanding failure episode, or zero
// and false when healthy. It never implies a Workflow has failed.
func (h *Hub) UnhealthySince() (time.Time, bool) {
	h.healthMu.Lock()
	defer h.healthMu.Unlock()
	since := h.healthSinceLocked()
	return since, !since.IsZero()
}

func (h *Hub) healthSinceLocked() time.Time {
	var since time.Time
	if !h.workflows.HasSynced() || !h.pods.HasSynced() {
		since = h.created
	}
	if !h.stopped.IsZero() && (since.IsZero() || h.stopped.Before(since)) {
		since = h.stopped
	}
	now := h.now()
	for _, state := range h.attempts {
		if state.failureSince.IsZero() || (!state.lastSuccess.IsZero() && now.Sub(state.lastSuccess) <= healthGrace) {
			continue
		}
		if since.IsZero() || state.failureSince.Before(since) {
			since = state.failureSince
		}
	}
	if since.IsZero() || h.unhealthySince.IsZero() {
		h.unhealthySince = since
	}
	return h.unhealthySince
}

func (h *Hub) watchErrorHandler(kind resourceKind) cache.WatchErrorHandlerWithContext {
	return func(ctx context.Context, _ *cache.Reflector, err error) {
		if ctx.Err() != nil {
			return
		}
		h.recordAttempt(kind, err)
		name := "Workflow"
		if kind == podKind {
			name = "Pod"
		}
		h.logger.WarnContext(ctx, "argowatch: observation failed", "kind", name, "error_class", errorClass(err))
	}
}

// Use a fixed vocabulary; raw API errors can contain object contents.
func errorClass(err error) string {
	switch {
	case apierrors.IsResourceExpired(err), apierrors.IsGone(err):
		return "expired"
	case apierrors.IsForbidden(err):
		return "forbidden"
	case apierrors.IsUnauthorized(err):
		return "unauthorized"
	case apierrors.IsTooManyRequests(err):
		return "too_many_requests"
	case apierrors.IsTimeout(err), apierrors.IsServerTimeout(err):
		return "timeout"
	case apierrors.IsServiceUnavailable(err):
		return "unavailable"
	default:
		return "transport_or_api"
	}
}
