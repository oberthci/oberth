package gitcache

import (
	"context"
	"errors"
	"github.com/oberthci/oberth/internal/sourcedelivery"
	"maps"
	"os"
)

// Once transport ends, each repository operation retries incomplete rollback
// under its gate. Active receives finish their own transaction.
func (c *Cache) restoreUnchecked(ctx context.Context, store string) (Reservation, []string, error) {
	files := c.receiveCleanupFor(store)
	if files != nil {
		if c.isActive(files.owner) {
			return Reservation{}, nil, nil
		}
		noteReadWait(ctx)
		if err := files.cleanReceive(); err != nil {
			return Reservation{}, nil, err
		}
	}
	record, lines, err := c.restoreReceiveRecord(ctx, store, files)
	if err == nil {
		c.forgetReceiveFiles(store, files)
	}
	return record, lines, err
}

func (c *Cache) restoreReceiveRecord(ctx context.Context, store string, files *receiveFiles) (Reservation, []string, error) {
	record, err := readReservation(store)
	if os.IsNotExist(err) {
		return Reservation{}, nil, nil
	}
	if err != nil {
		return Reservation{}, nil, err
	}
	if c.isActive("receive-" + record.Event.ID) {
		return Reservation{}, nil, nil
	}
	noteReadWait(ctx)
	defer sourcedelivery.Measure(ctx, sourcedelivery.Recovery, true)()
	return c.settleReceiveRecord(ctx, store, record, files != nil && files.rejected)
}

// Recovery and abandon retain only prepared, applied updates of an accepted receive.
func (c *Cache) settleReceiveRecord(ctx context.Context, store string, record Reservation, rejected bool) (Reservation, []string, error) {
	var lines []string
	if record.State == "ready" && !rejected {
		updates, restored, err := c.preparedUpdates(ctx, store, record)
		lines = restored
		if err != nil || len(updates) != 0 {
			record.Event.Updates = updates
			return record, lines, err
		}
	}
	_, err := c.rollbackReservation(ctx, store, record)
	return Reservation{}, lines, err
}
func markReceiveRejected(store string, record *Reservation) error {
	if record.State == "rejected" {
		return nil
	}
	rejected := *record
	rejected.State = "rejected"
	// If this write fails, refusal is memory-only: restart loses it and may
	// deliver the prepared portion recorded before the refusal.
	if err := writeReservation(store, rejected); err != nil {
		return err
	}
	*record = rejected
	return nil
}

func (c *Cache) rollbackReservation(ctx context.Context, store string, record Reservation) (string, error) {
	persistence := markReceiveRejected(store, &record)
	ref, err := c.changedRef(ctx, store, record.Before)
	if err == nil && ref != "" {
		err = c.rollbackReceiveRefs(ctx, store, record.Before)
	}
	if err == nil {
		err = c.finishReceiveLocked(ctx, store, record.Event.ID)
	}
	if err = errors.Join(persistence, err); err != nil {
		return ref, &receiveRollbackError{unavailable("restore unchecked ref "+ref, err)}
	}
	return ref, nil
}

func (c *Cache) changedRef(ctx context.Context, store string, before map[string]string) (string, error) {
	remaining := maps.Clone(before)
	changed := ""
	err := c.walkRefs(ctx, store, func(name, oid string) error {
		if before[name] != oid && changed == "" {
			changed = name
		}
		delete(remaining, name)
		return nil
	})
	if changed == "" {
		for name := range remaining {
			changed = name
			break
		}
	}
	return changed, err
}
