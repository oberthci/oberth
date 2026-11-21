package store

import (
	"errors"
	"testing"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/publicerr"
	"github.com/oberthci/oberth/internal/stateport"
	"github.com/oberthci/oberth/internal/stepmodel"
)

func TestPublicationSettlementCASAndSettledAbandonRefuse(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	p := publicationFixture(t, s)
	a := stateport.Action{Actor: "operator"}
	_, err := s.StartPublicationAttempt(t.Context(), a, stateport.PublicationAttempt{ID: p.ID})
	requireOK(t, err)
	requireCode(t, s.SettlePublication(t.Context(), a, stateport.PublicationResult{ID: p.ID, Attempt: 2, Delivered: true}), publicerr.Conflict)
	requireOK(t, s.SettlePublication(t.Context(), a, stateport.PublicationResult{ID: p.ID, Attempt: 1, Delivered: true}))
	requireCode(t, s.AbandonPublication(t.Context(), a, p.ID), publicerr.Conflict)
}

func TestExplicitPublicationRetryResetsBudgetAndPendingList(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	p := publicationFixture(t, s)
	a := stateport.Action{Actor: "operator"}
	_, err := s.StartPublicationAttempt(t.Context(), a, stateport.PublicationAttempt{ID: p.ID})
	requireOK(t, err)
	requireCode(t, s.RetryPublication(t.Context(), a, stateport.PublicationAttempt{ID: p.ID, ExpectedAttempts: 1}), publicerr.InvalidTransition)
	requireOK(t, s.SettlePublication(t.Context(), a, stateport.PublicationResult{ID: p.ID, Attempt: 1}))
	requireCode(t, s.RetryPublication(t.Context(), a, stateport.PublicationAttempt{ID: p.ID}), publicerr.Conflict)
	requireOK(t, s.RetryPublication(t.Context(), a, stateport.PublicationAttempt{ID: p.ID, ExpectedAttempts: 1}))
	read, err := s.Publication(t.Context(), p.ID)
	requireOK(t, err)
	if read.Attempts != 0 || read.RetryAt != nil || read.Status != model.PublicationPending {
		t.Fatal("retry did not reset budget", read)
	}
	list, err := s.PendingPublications(t.Context(), stateport.RecordFilter{Repositories: []string{p.Repo}, Limit: 200})
	requireOK(t, err)
	if len(list) != 1 || list[0].ID != p.ID {
		t.Fatal("pending list", list)
	}
}

func TestFinishReportsPendingPublicationOwnership(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	p := publicationFixture(t, s)
	r, err := s.Run(t.Context(), p.RunID)
	requireOK(t, err)
	err = s.Finish(t.Context(), stateport.Action{Actor: "operator"}, stateport.RunUpdate{ID: r.ID, Revision: r.Revision, Status: stepmodel.RunFailed})
	var refusal *publicerr.Error
	if !errors.As(err, &refusal) || refusal.Reason() != diagnostics()[pendingPublication].reason {
		t.Fatal("pending publication cause was lost", err)
	}
}

func TestFinishCommitsVerdictAndPublicationIntent(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	r, _ := accepted(t, s, "run")
	intent := publicationIntent(r)
	before := independentHead(t, s.readers)
	requireOK(t, s.Finish(t.Context(), stateport.Action{Actor: "operator"}, stateport.RunUpdate{ID: r.ID, Revision: r.Revision, Status: stepmodel.RunPassed, Publication: &intent}))
	p, err := s.Publication(t.Context(), intent.Publication.ID)
	requireOK(t, err)
	if p.RunID != r.ID || p.Status != model.PublicationPending || independentHead(t, s.readers).ID != before.ID+1 {
		t.Fatal("finish did not commit its audited publication", p)
	}
}
