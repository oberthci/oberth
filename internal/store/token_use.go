package store

import (
	"context"
	"fmt"
	"io"
	"time"
)

const tokenUseQueueSize = 64
const tokenUseFailureCooldown = time.Minute

type tokenUse struct {
	id string
	at time.Time
}

// RecordTokenCredentialUse queues optional last-used telemetry after a fresh
// authoritative authentication read. It never waits for SQLite. A full queue,
// shutdown, or a failed update drops telemetry. Pending IDs are deduplicated;
// failure starts a global one-minute cooldown before later requests may retry.
// The timestamp is accepted use time, not commit time. Never use it to authorize.
func (s *Store) RecordTokenCredentialUse(ctx context.Context, id string) {
	if ctx.Err() != nil {
		return
	}
	at := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.done || s.readOnly {
		return
	}
	if _, pending := s.pendingTokenUses[id]; pending {
		return
	}
	if at.Before(s.tokenUseRetryAt) {
		s.tokenUseCooldownDrops.Add(1)
		return
	}
	if s.tokenUses == nil {
		s.tokenUses = make(chan tokenUse, tokenUseQueueSize)
		s.pendingTokenUses = make(map[string]struct{})
		s.tokenUsesDone = make(chan struct{})
		workerCtx, cancel := context.WithCancel(context.Background())
		s.stopTokenUses = cancel
		go s.recordTokenUses(workerCtx)
	}
	select {
	case s.tokenUses <- tokenUse{id: id, at: at}:
		s.pendingTokenUses[id] = struct{}{}
	default:
		s.tokenUseQueueDrops.Add(1)
	}
}

func (s *Store) recordTokenUses(ctx context.Context) {
	defer func() {
		s.mu.Lock()
		clear(s.pendingTokenUses)
		s.mu.Unlock()
		close(s.tokenUsesDone)
	}()
	for {
		// Do not drain queued telemetry during shutdown. The UPDATE honors
		// cancellation while waiting for the serialized writer pool slot;
		// an external SQLite busy wait can still last until its busy timeout.
		if ctx.Err() != nil {
			return
		}
		select {
		case <-ctx.Done():
			return
		case use := <-s.tokenUses:
			s.mu.Lock()
			cooldown := s.now().Before(s.tokenUseRetryAt)
			if cooldown {
				delete(s.pendingTokenUses, use.id)
				s.tokenUseCooldownDrops.Add(1)
			}
			s.mu.Unlock()
			if cooldown {
				continue
			}
			// The conditional update still requires an active, unrevoked
			// credential and coalesces concurrent requests to one write/minute.
			s.tokenUseAttempts.Add(1)
			_, err := s.touchTokenCredentialAt(ctx, use.id, use.at)
			s.mu.Lock()
			delete(s.pendingTokenUses, use.id)
			if err != nil && ctx.Err() == nil {
				s.tokenUseFailures.Add(1)
				s.tokenUseRetryAt = s.now().Add(tokenUseFailureCooldown)
			}
			s.mu.Unlock()
		}
	}
}

// WriteTokenUseMetrics reports telemetry health without token identifiers.
func (s *Store) WriteTokenUseMetrics(w io.Writer) {
	for _, metric := range []struct {
		name  string
		value uint64
	}{
		{"attempts", s.tokenUseAttempts.Load()},
		{"failures", s.tokenUseFailures.Load()},
		{"queue_drops", s.tokenUseQueueDrops.Load()},
		{"cooldown_drops", s.tokenUseCooldownDrops.Load()},
	} {
		_, _ = fmt.Fprintf(w, "# TYPE oberth_token_use_%s_total counter\noberth_token_use_%s_total %d\n", metric.name, metric.name, metric.value)
	}
}
