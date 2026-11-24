package runlog

import (
	"context"
	"os"
)

// ReserveFinal retries an uncommitted archive of a caller-verified finished
// container. Ordinary Reserve remains single-attempt. Published spans never reopen.
func (s *Store) ReserveFinal(ctx context.Context, id string, entry ContainerLog) (*Reservation, error) {
	if err := s.retryFinal(ctx, id, entry); err != nil {
		return nil, err
	}
	return s.Reserve(ctx, id, entry)
}

func (s *Store) retryFinal(ctx context.Context, id string, entry ContainerLog) error {
	if err := checkID(id); err != nil {
		return err
	}
	if err := s.lock(ctx); err != nil {
		return err
	}
	defer s.unlock()
	saved, err := s.load(id)
	if err != nil {
		return err
	}
	at := position(saved, entry)
	if at < 0 || saved.Containers[at].State != Copying {
		return nil
	}
	if err := samePhysical(saved.Containers[at], entry); err != nil {
		return err
	}
	if _, active := s.active.Load(copyKey(id, entry)); active {
		return nil
	}
	saved.Containers[at].State = Waiting
	return s.save(id, saved)
}

// Archived checks durable receipts and their body before native cleanup.
// Captured historical gaps are final evidence; unpublished copies still block.
func (s *Store) Archived(ctx context.Context, id string, entries []ContainerLog) (bool, error) {
	if err := checkID(id); err != nil {
		return false, err
	}
	if err := s.lock(ctx); err != nil {
		return false, err
	}
	defer s.unlock()
	saved, err := s.load(id)
	if err != nil {
		return false, err
	}
	if ready, err := archivedEntries(saved, entries); !ready || err != nil {
		return false, err
	}
	if len(entries) == 0 {
		return true, nil
	}
	body, err := s.openBody(id, os.O_RDONLY)
	if err != nil {
		return false, err
	}
	defer func() { _ = body.Close() }()
	info, err := body.Stat()
	if err != nil || info.Size() < saved.Size {
		return false, err
	}
	// A previous rename may have become visible before its directory sync failed.
	// Confirm that durability boundary again before authorizing source collection.
	if err := s.syncDirectory(); err != nil {
		return false, err
	}
	return true, nil
}

type finalReservationStore struct {
	store interface {
		ReserveFinal(context.Context, string, ContainerLog) (*Reservation, error)
	}
}

func (s finalReservationStore) Reserve(ctx context.Context, id string, entry ContainerLog) (*Reservation, error) {
	return s.store.ReserveFinal(ctx, id, entry)
}

// Archive consumes a terminated container, retaining the existing aggregate cap.
// Incomplete reads leave an uncommitted receipt eligible for a later archive.
func Archive(ctx context.Context, store interface {
	ReserveFinal(context.Context, string, ContainerLog) (*Reservation, error)
}, in SourceCapture) error {
	in.Complete = true
	return Capture(ctx, finalReservationStore{store}, in)
}

func archivedEntries(saved index, entries []ContainerLog) (bool, error) {
	values := make(map[captureKey]ContainerLog, len(saved.Containers))
	for _, entry := range saved.Containers {
		values[copyKey(saved.RunID, entry)] = entry
	}
	for _, entry := range entries {
		value, ok := values[copyKey(saved.RunID, entry)]
		if !ok {
			return false, nil
		}
		if err := samePhysical(value, entry); err != nil {
			return false, err
		}
		if value.NodeID != entry.NodeID || value.State != Captured {
			return false, nil
		}
	}
	return true, nil
}

// RecordGap durably closes a terminal source that no longer exists. Existing
// captured output, including an explicit partial receipt, is never overwritten.
func (s *Store) RecordGap(ctx context.Context, id string, entry ContainerLog, gap Gap) error {
	r, err := s.ReserveFinal(ctx, id, entry)
	if err != nil || r == nil {
		return err
	}
	return r.Capture(ctx, CaptureRequest{Gap: gap})
}
