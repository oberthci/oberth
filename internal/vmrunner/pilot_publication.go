package vmrunner

import (
	"context"
	"errors"
)

// PilotReceiptSource provides only cleanup-qualified host evidence. This
// interface is intentionally not connected to a scheduler capability yet.
type PilotReceiptSource interface {
	CompletedPilotReceipt(context.Context, string) (PilotReceipt, error)
}

func VerifyPilotPublication(ctx context.Context, source PilotReceiptSource, sealed PilotPlan) (PilotReceipt, error) {
	if err := ValidatePilotPlan(sealed); err != nil {
		return PilotReceipt{}, err
	}
	if source == nil {
		return PilotReceipt{}, errors.New("vmrunner: protected pilot evidence is unavailable")
	}
	receipt, err := source.CompletedPilotReceipt(ctx, sealed.Spec.RunID)
	if err != nil {
		return PilotReceipt{}, err
	}
	if err := VerifyPilotReceipt(sealed, receipt); err != nil {
		return PilotReceipt{}, err
	}
	return receipt, nil
}
