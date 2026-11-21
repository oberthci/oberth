package gitcache

import "context"

// Preparation records names; actualUpdates already requires each admitted new value.
func (c *Cache) preparedUpdates(ctx context.Context, store string, record Reservation) ([]RefUpdate, []string, error) {
	applied, _, err := c.actualUpdates(ctx, store, record)
	if err != nil {
		return nil, nil, err
	}
	var accepted, rollback []RefUpdate
	for _, update := range applied {
		_, prepared := record.Prepared[update.Ref]
		if prepared {
			accepted = append(accepted, update)
		} else {
			rollback = append(rollback, RefUpdate{Ref: update.Ref, OldSHA: update.NewSHA, NewSHA: record.Before[update.Ref]})
		}
	}
	if len(accepted) == 0 {
		return nil, c.restoredReceiveFeedback(rollback, true), nil
	}
	if err = c.refTransaction(ctx, store, rollback); err != nil {
		return nil, nil, &receiveRepairError{unavailable("restore unprepared references", err)}
	}
	return accepted, c.restoredReceiveFeedback(rollback, len(accepted) == 0), nil
}

func (c *Cache) restoredReceiveFeedback(updates []RefUpdate, refused bool) []string {
	reason := reasonReceiveRestored
	if refused {
		reason = reasonUnchecked
	}
	var lines []string
	for _, update := range updates {
		if c.config.Log != nil {
			c.config.Log(reasonReceiveRestored, update.Ref)
		}
		lines = append(lines, reason+". "+remedyUnchecked+" Reference: "+update.Ref+".")
	}
	return lines
}
