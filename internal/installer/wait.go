package installer

import (
	"context"
	"errors"
	"time"
)

// ReadinessTimeout allows an image pull and database migration on a loaded node.
const ReadinessTimeout = 10 * time.Minute

func (e *Engine) waitReady(ctx context.Context, p Profile, previousURL string) error {
	var last error
	for {
		if err := ctx.Err(); err != nil {
			return readinessFailure(err, last)
		}
		err := e.probeReady(ctx, p)
		last = err
		// A connection attempt can time out while the rollout still has time.
		// Only the readiness context owns the command's deadline.
		if ctx.Err() != nil {
			return readinessFailure(ctx.Err(), last)
		}
		if err == nil {
			return nil
		}
		if !e.rolloutPending(err, p, previousURL) {
			return err
		}
		if err = e.pause(ctx); err != nil {
			return readinessFailure(err, last)
		}
	}
}

func (e *Engine) rolloutPending(err error, p Profile, previousURL string) bool {
	var refusal *Problem
	if !errors.As(err, &refusal) {
		return false
	}
	switch refusal.Code {
	case "starting", "connect", "ready-version":
		return true
	case "tls":
		return e.previousServerCertificate(err, p, previousURL)
	}
	return false
}

func readinessFailure(cause, last error) error {
	var refusal *Problem
	if errors.Is(cause, context.DeadlineExceeded) && errors.As(last, &refusal) && refusal.Code == "ready-version" {
		return described("version-timeout", cause, refusal.Detail)
	}
	return problem("timeout", cause)
}

func (e *Engine) probeReady(ctx context.Context, p Profile) error {
	probe, cancel := context.WithTimeout(ctx, targetProbe)
	defer cancel()
	return e.CheckEndpoint(probe, p)
}

// pause is one step of the readiness wait; an injected Pause replaces it in tests.
func (e *Engine) pause(ctx context.Context) error {
	if e.Pause != nil {
		return e.Pause(ctx)
	}
	return pauseReady(ctx)
}
func pauseReady(ctx context.Context) error {
	timer := time.NewTimer(250 * time.Millisecond)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
