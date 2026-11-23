package installer

import (
	"context"
	"errors"
	"time"
)

// ReadinessTimeout allows an image pull and database migration on a loaded node.
const ReadinessTimeout = 10 * time.Minute

func (e *Engine) waitReady(ctx context.Context, p Profile) error {
	for {
		if err := ctx.Err(); err != nil {
			return problem("timeout", err)
		}
		err := e.probeReady(ctx, p)
		// A connection attempt can time out while the rollout still has time.
		// Only the readiness context owns the command's deadline.
		if ctx.Err() != nil {
			return problem("timeout", ctx.Err())
		}
		if err == nil {
			return nil
		}
		var refusal *Problem
		if !errors.As(err, &refusal) || refusal.Code != "starting" && refusal.Code != "connect" {
			return err
		}
		if err = e.pause(ctx); err != nil {
			return problem("timeout", err)
		}
	}
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
