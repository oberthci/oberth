package service

import (
	"context"
	"strings"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/publicerr"
	"github.com/oberthci/oberth/internal/stateport"
	"github.com/oberthci/oberth/internal/stepmodel"
)

func (s *Service) publicCancellation(ctx context.Context, r stateport.Run, v *stepmodel.RunSummary) error {
	if r.Status != stepmodel.RunCancelled {
		return nil
	}
	obligations, err := s.c.Runs.Obligations(ctx, r.ID)
	if err != nil {
		return err
	}
	for _, obligation := range obligations {
		if obligation.Kind != "stop" || v.DisplayReason == stateport.ReasonBranchDeleted {
			continue
		}
		v.Detail = strings.TrimSpace(v.Detail + "\nCancelled by " + obligation.Actor + ".")
		reason := "cancelled by " + obligation.Actor
		v.DisplayReason = reasonCancelled
		if publicerr.CheckMessage(reason, 40) == nil {
			v.DisplayReason = reason
		}
	}
	remedy := detailCancelled
	if r.Origin.Trigger == model.TriggerPromotion {
		remedy = detailCancelledPromotion
	}
	v.Detail = strings.TrimSpace(v.Detail + "\n" + remedy)
	return nil
}
