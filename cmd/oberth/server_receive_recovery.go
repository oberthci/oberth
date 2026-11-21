package main

import (
	"context"
	"log/slog"
	"time"
)

func (o *serverOwners) recoverPendingReceives(ctx context.Context) error {
	recovery, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	replayErr := o.cache.ReplayPending(recovery, o.deliver)
	records, err := o.cache.PendingReceives(ctx)
	logger := slog.New(slog.NewTextHandler(o.c.diagnostic, nil))
	for _, r := range records {
		if r.Problem != "" {
			logger.Warn("receive set aside", "repository", r.Event.Repository, "receive", r.Event.ID, "reason", r.Problem)
		}
	}
	if err != nil || replayErr != nil && len(records) == 0 {
		logger.Warn("receive inspection unavailable", "replay", replayErr, "inspection", err)
	}
	return ctx.Err()
}
