package installer

import (
	"context"
	"fmt"
	"net/http"

	"github.com/oberthci/oberth/internal/secretstore"
)

// Unsealed Raft storage may still be electing a leader. Only native active
// health permits bootstrap writes; authentication/configuration failures are
// never retried. This wait shares the chart/store phase's existing deadline.
func (e *Engine) waitStoreActive(ctx context.Context, c *secretstore.Client) error {
	pause := e.Pause
	if pause == nil {
		pause = pauseReady
	}
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		status, err := c.JSON(ctx, secretstore.Request{Method: "GET", Path: "/sys/health"}, nil)
		switch status {
		case http.StatusOK:
			return err
		case http.StatusTooManyRequests, http.StatusServiceUnavailable:
			if err = pause(ctx); err != nil {
				return err
			}
		default:
			if status == 0 {
				return err
			}
			return described("store-health", err, fmt.Sprintf("Secret store /sys/health: HTTP %d", status))
		}
	}
}
