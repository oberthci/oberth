package main

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/oberthci/oberth/internal/model"
)

const (
	idleSnapshotReadBudget = 5 * time.Second
	idleSnapshotInterval   = time.Second
	// Cover a full read plus interval and leave margin for slow I/O.
	idleSnapshotMaxAge = 10 * time.Second
)

// idleQueueSnapshot keeps admin probes off the database's single connection.
// Samples remain advisory; failed, timed-out or stale reads cannot report idle.
// Like the readiness sampler, only the background observer does storage I/O.
type idleQueueSnapshot struct {
	mu       sync.Mutex
	state    model.IdleQueueStatus
	observed time.Time
	valid    bool
}

func (s *idleQueueSnapshot) current(ctx context.Context) (model.IdleQueueStatus, error) {
	if err := ctx.Err(); err != nil {
		return model.IdleQueueStatus{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.valid || time.Since(s.observed) >= idleSnapshotMaxAge {
		return model.IdleQueueStatus{}, errors.New("live queue snapshot unavailable or stale")
	}
	return s.state, nil
}

func (s *idleQueueSnapshot) watch(ctx context.Context, read func(context.Context) (model.IdleQueueStatus, error)) {
	for {
		if ctx.Err() != nil {
			return
		}
		// Timestamp the start, so a slow read cannot publish old idle state as
		// fresh. Expiry also applies while a replacement sample is blocked.
		observed := time.Now()
		probeCtx, cancel := context.WithTimeout(ctx, idleSnapshotReadBudget)
		state, err := read(probeCtx)
		valid := err == nil && probeCtx.Err() == nil && state.Valid()
		cancel()
		s.mu.Lock()
		s.state, s.observed, s.valid = state, observed, valid
		s.mu.Unlock()
		timer := time.NewTimer(idleSnapshotInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}
