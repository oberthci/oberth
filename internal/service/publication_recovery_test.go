package service_test

import (
	"context"
	"testing"

	"github.com/oberthci/oberth/internal/gitport"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/stateport"
	"github.com/oberthci/oberth/internal/stepmodel"
)

func TestPublicationRecoveryBindsInterruptedPreflight(t *testing.T) {
	f := setup(t)
	p := finishPublication(t, f, f.seed(stepmodel.RunRunning))
	if _, err := f.store.StartPublicationAttempt(t.Context(), stateport.Action{Actor: p.Actor}, stateport.PublicationAttempt{ID: p.ID}); err != nil {
		t.Fatal(err)
	}
	f.moving.Remote = func(context.Context, string, string) (gitport.Ref, error) {
		return gitport.Ref{Name: p.Ref, ObjectSHA: p.ObjectSHA, CommitSHA: *p.CommitSHA}, nil
	}
	if err := f.s.RecoverPublications(t.Context()); err != nil {
		t.Fatal(err)
	}
	p, _ = f.store.Publication(t.Context(), p.ID)
	if p.Status != model.PublicationDelivered || p.PreviousSHA == nil || *p.PreviousSHA != p.ObjectSHA {
		t.Fatal("recovery skipped predecessor binding", p)
	}
}
