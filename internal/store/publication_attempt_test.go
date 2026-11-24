package store

import (
	"testing"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/stateport"
)

func TestPublicationAttemptEvidenceSurvivesRestart(t *testing.T) {
	s := testStore(t)
	atomicRepository(t, s)
	p := model.Publication{ID: "restart-attempt", Repo: "origin/team/repo", Ref: "refs/heads/main", Actor: "operator", ObjectSHA: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}
	a := stateport.Action{Actor: p.Actor}
	requireOK(t, s.PreparePublication(t.Context(), a, stateport.PublicationIntent{Publication: p}))
	_, err := s.StartPublicationAttempt(t.Context(), a, stateport.PublicationAttempt{ID: p.ID})
	requireOK(t, err)
	requireOK(t, s.SettlePublication(t.Context(), a, stateport.PublicationResult{ID: p.ID, Attempt: 1}))
	requireOK(t, s.RetryPublication(t.Context(), a, stateport.PublicationAttempt{ID: p.ID, ExpectedAttempts: 1}))
	path, options := s.path, s.options
	requireOK(t, s.Close())
	s, err = Open(t.Context(), path, options)
	requireOK(t, err)
	defer func() { requireOK(t, s.Close()) }()
	got, err := s.Publication(t.Context(), p.ID)
	requireOK(t, err)
	assertEmptyAttemptEvidence(t, got, 0)
	listed, err := s.PendingPublications(t.Context(), stateport.RecordFilter{Limit: 10})
	requireOK(t, err)
	if len(listed) != 1 {
		t.Fatal("restart lost pending publication", listed)
	}
	assertEmptyAttemptEvidence(t, listed[0], 0)
	accepted(t, s, "after-restart")
	got, err = s.StartPublicationAttempt(t.Context(), a, stateport.PublicationAttempt{ID: p.ID})
	requireOK(t, err)
	assertEmptyAttemptEvidence(t, got, 1)
	got, err = s.BindPublication(t.Context(), a, p.ID, "")
	requireOK(t, err)
	assertEmptyAttemptEvidence(t, got, 1)
}

func assertEmptyAttemptEvidence(t *testing.T, p model.Publication, attempts int) {
	t.Helper()
	if !p.Attempted || p.CIAfter != 0 || p.Attempts != attempts {
		t.Fatal("restart or bind changed private attempt evidence", p)
	}
}
