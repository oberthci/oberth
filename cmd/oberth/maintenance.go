package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"time"
)

func (h *health) retainEvents(ctx context.Context, retain func(context.Context, time.Time, time.Time) error, out io.Writer) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-h.readySignal:
	}
	ticks, stop := h.clock.Ticker(5 * time.Minute)
	defer func() { stop() }()
	first := true
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticks:
			now := h.clock.Now()
			h.maintenanceFailure("retention", retain(ctx, now.Add(-30*24*time.Hour), now.Add(-24*time.Hour)), out)
			if first {
				stop()
				ticks, stop = h.clock.Ticker(24 * time.Hour)
				first = false
			}
		}
	}
}
func (c *composition) maintainCache(ctx context.Context) error {
	logger := slog.New(slog.NewTextHandler(c.diagnostic, nil))
	ticks, stop := c.clock.Ticker(time.Hour)
	defer stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case notice := <-c.cacheNotices:
			logger.Info("Git cache observation", "reason", notice.reason, "detail", notice.detail)
		case <-ticks:
			c.health.maintenanceFailure("cache", c.cacheGC(ctx), c.diagnostic)
		}
	}
}

// Each failed tick is observable without ending the owning lifetime. The cache
// error carries repository paths, including every failure in a joined error.
func (h *health) maintenanceFailure(kind string, err error, out io.Writer) {
	if err == nil {
		return
	}
	h.mu.Lock()
	if kind == "retention" {
		h.retentionFailures++
	} else {
		h.cacheFailures++
	}
	h.mu.Unlock()
	m := serverMessages()[kind]
	_, _ = fmt.Fprintf(out, "%s %s Detail: %q\n", m[0], m[1], err)
}
