package installer

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"time"

	"github.com/oberthci/oberth/internal/client"
	"github.com/oberthci/oberth/internal/model"
)

// WaitForAdminIdle observes the complete queue with an authenticated unscoped admin.
func WaitForAdminIdle(ctx context.Context, remote AdminAPI, mode string, timeout time.Duration, output io.Writer) error {
	if mode != "all" && mode != "critical" || timeout <= 0 {
		return errors.New("invalid admin idle observation options")
	}
	if output == nil {
		output = io.Discard
	}
	return observeAdminIdle(ctx, mode, timeout, Deps{Admin: remote, Output: output, PollInterval: time.Second})
}
func observeAdminIdle(ctx context.Context, mode string, limit time.Duration, deps Deps) error {
	if limit == 0 {
		limit = DefaultUpgradeWaitTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, limit)
	defer cancel()
	start := time.Now()
	nextProgress := time.Time{}
	var lastErr error
	for {
		var result struct {
			Busy  bool                  `json:"busy"`
			Queue model.IdleQueueStatus `json:"queue"`
		}
		err := CallAdmin(ctx, Config{}, deps, "idle", map[string]string{"wait_for": mode}, &result)
		if err == nil {
			if !result.Queue.Valid() {
				return errors.New("invalid admin idle observation")
			}
			if !result.Busy {
				return nil
			}
			lastErr = nil
		} else {
			var status *client.AdminError
			var network net.Error
			if errors.As(err, &status) {
				if status.StatusCode < 500 && status.StatusCode != 429 {
					return err
				}
			} else if !errors.As(err, &network) && !errors.Is(err, context.DeadlineExceeded) {
				return err
			}
			lastErr = err
		}
		if !time.Now().Before(nextProgress) {
			if _, err := fmt.Fprintf(deps.Output, "Waiting for %s queue: preparing=%d running=%d critical=%d elapsed=%s\n", mode, result.Queue.Preparing, result.Queue.Running, result.Queue.Critical(), time.Since(start).Truncate(time.Second)); err != nil {
				return err
			}
			nextProgress = time.Now().Add(15 * time.Second)
		}
		if err := sleepContext(ctx, pollInterval(deps)); err != nil {
			return fmt.Errorf("wait for %s queue via admin API (elapsed=%s limit=%s): %w", mode, time.Since(start).Truncate(time.Millisecond), limit, errors.Join(err, lastErr))
		}
	}
}
