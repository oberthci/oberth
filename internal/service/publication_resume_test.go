package service_test

import (
	"context"
	"testing"

	"github.com/oberthci/oberth/internal/gitport"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/stateport"
	"github.com/oberthci/oberth/internal/stepmodel"
)

func TestRecoveryReadsBackAnInterruptedFirstAttempt(t *testing.T) {
	for _, bound := range []bool{false, true} {
		t.Run(map[bool]string{false: "unbound", true: "bound"}[bound], func(t *testing.T) { checkFirstAttemptRecovery(t, bound) })
	}
}

func interruptedFirstPublication(t *testing.T, bound bool) (*fixture, model.Publication) {
	t.Helper()
	f := setup(t)
	p := finishPublication(t, f, f.seed(stepmodel.RunRunning))
	p, err := f.store.StartPublicationAttempt(t.Context(), stateport.Action{Actor: p.Actor}, stateport.PublicationAttempt{ID: p.ID})
	if err != nil {
		t.Fatal(err)
	}
	if bound {
		p, err = f.store.BindPublication(t.Context(), stateport.Action{Actor: p.Actor}, p.ID, "")
		if err != nil {
			t.Fatal(err)
		}
	}
	return f, p
}

func checkFirstAttemptRecovery(t *testing.T, bound bool) {
	t.Helper()
	f, p := interruptedFirstPublication(t, bound)
	f.moving.Remote = func(context.Context, string, string) (gitport.Ref, error) {
		return gitport.Ref{Name: p.Ref, ObjectSHA: p.ObjectSHA, CommitSHA: *p.CommitSHA}, nil
	}
	called := false
	f.moving.Push = func(_ context.Context, resumed model.Publication) (gitport.PublicationReceipt, error) {
		called = true
		if !resumed.ReadBack || resumed.Attempts != 1 {
			t.Fatal("recovery lost first-attempt read-back mode", resumed)
		}
		return gitport.PublicationReceipt{Repository: p.Repo, ActionReceipt: p.ID, Refs: []gitport.Ref{{Name: p.Ref, ObjectSHA: p.ObjectSHA, CommitSHA: *p.CommitSHA}}}, nil
	}
	if err := f.s.RecoverPublications(t.Context()); err != nil {
		t.Fatal(err)
	}
	stored, err := f.store.Publication(t.Context(), p.ID)
	if err != nil || !called || stored.Attempts != 1 || stored.Status != model.PublicationDelivered || stored.ReadBack {
		t.Fatal("resuming changed durable attempt facts", stored, err)
	}
}
