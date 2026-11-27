package main

import (
	"context"

	"github.com/oberthci/oberth/internal/logcontract"
	"github.com/oberthci/oberth/internal/stateport"
)

func (l serverLogs) Usage(ctx context.Context, r stateport.Run) (logcontract.Usage, error) {
	usage, err := l.owner.c.logs.Usage(ctx, r.ID, l.owner.c.clock.Now())
	if err != nil {
		return usage, err
	}
	usage.ProtectionReason, err = l.owner.c.store.LogProtection(ctx, r.ID)
	if err != nil {
		return usage, err
	}
	receipt, err := l.owner.c.store.PurgeReceipt(ctx, r.ID)
	if missingStore(err) {
		return usage, nil
	}
	if err != nil {
		return usage, err
	}
	usage.PurgePending, usage.PurgeCompleted = !receipt.Completed, receipt.Completed
	return usage, nil
}
