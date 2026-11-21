package service_test

import (
	"context"
	"testing"

	"github.com/oberthci/oberth/internal/gitport"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/stepmodel"
)

func TestPublicationReceiptBindsDurableAction(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"same action", "missing action", "another action"} {
		t.Run(name, func(t *testing.T) { checkPublicationReceiptAction(t, name) })
	}
}

func checkPublicationReceiptAction(t *testing.T, name string) {
	t.Helper()
	f := setup(t)
	p := finishPublication(t, f, f.seed(stepmodel.RunRunning))
	action := p.ID
	switch name {
	case "missing action":
		action = ""
	case "another action":
		action = "another-publication"
	}
	f.moving.Push = func(context.Context, model.Publication) (gitport.PublicationReceipt, error) {
		return gitport.PublicationReceipt{Repository: p.Repo, ActionReceipt: action, Refs: []gitport.Ref{{Name: p.Ref, ObjectSHA: p.ObjectSHA, CommitSHA: *p.CommitSHA}}}, nil
	}
	if err := f.s.Deliver(t.Context(), p.ID); err != nil {
		t.Fatal(err)
	}
	got, err := f.store.Publication(t.Context(), p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if name == "same action" {
		if got.Status != model.PublicationDelivered {
			t.Fatal("matching receipt did not confirm delivery", got)
		}
		return
	}
	want := "unavailable: The upstream receipt does not confirm this publication. - Read publication_list and retry the exact intent."
	if got.Status != model.PublicationPending || got.RetryAt == nil || got.Detail != want {
		t.Fatal("unconfirmed action did not retain its retry and explanation", got)
	}
}
