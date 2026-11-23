package service

import (
	"context"
	"strings"

	"github.com/oberthci/oberth/internal/publicerr"
	"github.com/oberthci/oberth/internal/stepmodel"
)

// The durable cancellation names the caller, even after observation restarts.
func (s *Service) cancellationReason(ctx context.Context, o *FinishedRun) error {
	if o.Status != stepmodel.RunCancelled {
		return nil
	}
	obligations, err := s.c.Runs.Obligations(ctx, o.RunID)
	if err != nil {
		return err
	}
	for _, obligation := range obligations {
		if obligation.Kind != "stop" {
			continue
		}
		o.Reason = "cancelled by " + obligation.Actor
		if publicerr.CheckMessage(o.Reason, 40) != nil {
			o.Reason = reasonCancelled
		}
		o.Detail = strings.TrimSpace("Cancelled by " + obligation.Actor + ".\n" + o.Detail)
	}
	return nil
}
