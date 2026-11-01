package store

import "context"

const tokenUseQueueSize = 64

// RecordTokenCredentialUse queues optional last-used telemetry after a fresh
// authoritative authentication read. It never waits for SQLite. A full queue,
// shutdown, or a failed update drops telemetry; a later request retries while
// the committed timestamp remains due. It must not be used to authorize work.
func (s *Store) RecordTokenCredentialUse(ctx context.Context, id string) {
	if ctx.Err() != nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.done || s.readOnly {
		return
	}
	if s.tokenUses == nil {
		s.tokenUses = make(chan string, tokenUseQueueSize)
		s.tokenUsesDone = make(chan struct{})
		workerCtx, cancel := context.WithCancel(context.Background())
		s.stopTokenUses = cancel
		go s.recordTokenUses(workerCtx)
	}
	select {
	case s.tokenUses <- id:
	default:
	}
}

func (s *Store) recordTokenUses(ctx context.Context) {
	defer close(s.tokenUsesDone)
	for {
		// Do not drain queued telemetry during shutdown. Touch honors this
		// context when waiting for the serialized writer connection too.
		if ctx.Err() != nil {
			return
		}
		select {
		case <-ctx.Done():
			return
		case id := <-s.tokenUses:
			// The conditional update still requires an active, unrevoked
			// credential and coalesces concurrent requests to one write/minute.
			_ = s.TouchTokenCredential(ctx, id)
		}
	}
}
