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
		err := e.CheckEndpoint(ctx, p)
		if err == nil {
			return nil
		}
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return problem("timeout", err)
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
