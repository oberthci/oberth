package client

import (
	"context"
	"time"

	"github.com/oberthci/oberth/internal/restartretry"
)

// WaitFor uses the injected wait or the cancellable client default.
func (c Config) WaitFor(ctx context.Context, delay time.Duration) error {
	if c.Wait != nil {
		return c.Wait(ctx, delay)
	}
	return restartretry.Wait(ctx, delay)
}

// Time uses the injected clock or the current time.
func (c Config) Time() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now() //nolint:forbidigo // The client configuration owns its zero-value clock default.
}
