package gitcache

import (
	"context"
	"errors"
	"maps"
	"os/exec"
)

// The receive slot and joined gate give completion sole ownership of the record.
// Persist rejection before any repository wait or fallible reference inspection.
func (c *Cache) completeReceive(ctx context.Context, store string, files *receiveFiles, result *receiveCompletion) error {
	record, err := readReservation(store)
	if err != nil {
		return err
	}
	accepted := result.accepts(record)
	if accepted && !result.failed() {
		result.accepted = true
		return nil
	}
	var persistence, refusal error
	if !accepted {
		files.rejected = true
		refusal = receiveRejection(record)
		persistence = markReceiveRejected(store, &record)
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), c.budgets.cleanup)
	defer cancel()
	if err = c.lock(ctx, store); err != nil {
		return errors.Join(persistence, err)
	}
	defer c.unlock(store)
	var cleanup error
	if result.failed() {
		cleanup = files.cleanReceive()
	}
	if accepted {
		updates, lines, inspectErr := c.preparedUpdates(ctx, store, record)
		result.feedback = lines
		if inspectErr != nil || len(updates) != 0 {
			result.accepted = inspectErr == nil
			return errors.Join(receiveCleanupResult(cleanup, result.accepted), inspectErr)
		}
		persistence = markReceiveRejected(store, &record)
	}
	ref, rollback := c.rollbackReservation(ctx, store, record)
	if ref != "" && refusal == nil {
		refusal = &RefusalError{reason: reasonUnchecked, remedy: remedyUnchecked, Detail: ref}
	}
	return errors.Join(persistence, cleanup, refusal, rollback)
}

func (result receiveCompletion) accepts(record Reservation) bool {
	return record.State == "ready" && record.Prepared != nil && !result.refused()
}

func (result receiveCompletion) failed() bool { return result.err != nil || !result.returned }

func (result receiveCompletion) refused() bool {
	var exit *exec.ExitError
	native := errors.As(result.err, &exit) && exit.ExitCode() > 0
	return result.gate == nil || result.gate.refused || native
}

func (c *Cache) rollbackReceiveRefs(ctx context.Context, store string, before map[string]string) error {
	remaining := maps.Clone(before)
	batch := make([]RefUpdate, 0, MaxUpdates)
	flush := func() error { err := c.refTransaction(ctx, store, batch); batch = batch[:0]; return err }
	err := c.walkRefs(ctx, store, func(name, oid string) error {
		if before[name] == oid {
			delete(remaining, name)
			return nil
		}
		batch = append(batch, RefUpdate{Ref: name, OldSHA: oid})
		if len(batch) == MaxUpdates {
			return flush()
		}
		return nil
	})
	if err != nil {
		return err
	}
	if err = flush(); err != nil {
		return err
	}
	for name, oid := range remaining {
		batch = append(batch, RefUpdate{Ref: name, NewSHA: oid})
		if len(batch) == MaxUpdates {
			if err = flush(); err != nil {
				return err
			}
		}
	}
	return flush()
}
func receiveRejection(record Reservation) error {
	switch record.State {
	case "ready":
		return &RefusalError{reason: reasonUnchecked, remedy: remedyUnchecked}
	case "rejected":
		return &reportedError{refuse("receive gate failed")}
	}
	return nil
}

func receiveCleanupResult(err error, accepted bool) error {
	if accepted && err != nil {
		return &receiveCleanupPendingError{err}
	}
	return err
}
