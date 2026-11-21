package service_test

import (
	"context"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/gitport"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/stateport"
	"github.com/oberthci/oberth/internal/stepmodel"
)

func TestPublicationReuseRequiresMatchingPeeledCommit(t *testing.T) {
	t.Parallel()
	for _, commit := range []string{head, strings.Repeat("d", 40)} {
		t.Run(commit, func(t *testing.T) { checkPublicationReuseCommit(t, commit) })
	}
}

func checkPublicationReuseCommit(t *testing.T, commit string) {
	t.Helper()
	f := setup(t)
	r := f.seed(stepmodel.RunRunning)
	r.Ref, r.ObjectSHA = "refs/tags/v1.2.3", strings.Repeat("c", 40)
	r.Origin.Trigger, r.Release = model.TriggerTag, true
	old := finishPublication(t, f, r)
	if err := f.s.Deliver(t.Context(), old.ID); err != nil {
		t.Fatal(err)
	}
	p := model.Publication{ID: "reuse-release", Repo: repository, Ref: r.Ref, ObjectSHA: r.ObjectSHA, CommitSHA: &commit, Actor: caller().Identity}
	if err := f.store.PreparePublication(t.Context(), stateport.Action{Actor: p.Actor}, stateport.PublicationIntent{Publication: p}); err != nil {
		t.Fatal(err)
	}
	f.moving.Push = func(context.Context, model.Publication) (gitport.PublicationReceipt, error) {
		t.Fatal("reusing release evidence must not push the tag again")
		return gitport.PublicationReceipt{}, nil
	}
	if err := f.s.Deliver(t.Context(), p.ID); err != nil {
		t.Fatal(err)
	}
	got, err := f.store.Publication(t.Context(), p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if commit == head {
		if got.Status != model.PublicationDelivered {
			t.Fatal("matching release evidence was not reused", got)
		}
		return
	}
	want := "forbidden: This run cannot provide publication evidence. - Use an unassisted passing run for the exact commit."
	if got.Status != model.PublicationFailed || got.Detail != want {
		t.Fatal("conflicting peeled commit was not refused", got)
	}
}
