package store

import (
	"testing"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/stateport"
)

type existingAttemptCase struct {
	name      string
	attempts  int
	after     int64
	attempted bool
}

func TestExistingSchema6PublicationAttemptEvidence(t *testing.T) {
	for _, test := range []existingAttemptCase{
		{"unattempted", 0, 0, false},
		{"started-empty", 1, 0, true},
		{"reset-empty", 0, -1, true},
		{"started-history", 1, 1, true},
		{"reset-history", 0, 1, true},
	} {
		t.Run(test.name, func(t *testing.T) { checkExistingPublicationAttempt(t, test) })
	}
}

func checkExistingPublicationAttempt(t *testing.T, test existingAttemptCase) {
	t.Helper()
	s := testStore(t)
	atomicRepository(t, s)
	if test.after > 0 {
		accepted(t, s, "before-attempt")
	}
	p := model.Publication{ID: "existing-attempt", Repo: "origin/team/repo", Ref: "refs/heads/main", Actor: "operator", ObjectSHA: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}
	a := stateport.Action{Actor: p.Actor}
	requireOK(t, s.PreparePublication(t.Context(), a, stateport.PublicationIntent{Publication: p}))
	// Reproduce the existing schema 6 row encoding, without the new private flag.
	_, err := s.writer.ExecContext(t.Context(), `UPDATE publications SET value=json_remove(json_set(value,'$.attempts',?,'$.ci_after',?),'$.attempted') WHERE id=?`, test.attempts, test.after, p.ID)
	requireOK(t, err)
	requireExistingAttemptDecoding(t, s, p.ID, test)
	accepted(t, s, "after-attempt")
	if test.attempts > 0 {
		requireOK(t, s.SettlePublication(t.Context(), a, stateport.PublicationResult{ID: p.ID, Attempt: test.attempts}))
		requireOK(t, s.RetryPublication(t.Context(), a, stateport.PublicationAttempt{ID: p.ID, ExpectedAttempts: test.attempts}))
	}
	got, err := s.StartPublicationAttempt(t.Context(), a, stateport.PublicationAttempt{ID: p.ID})
	requireOK(t, err)
	after := max(0, test.after)
	if !test.attempted {
		after = 1
	}
	if !got.Attempted || got.CIAfter != after {
		t.Fatal("retry recaptured existing history or new intent skipped binding", got)
	}
}

func requireExistingAttemptDecoding(t *testing.T, s *Store, id string, test existingAttemptCase) {
	t.Helper()
	got, err := s.Publication(t.Context(), id)
	requireOK(t, err)
	if got.Attempted != test.attempted || got.CIAfter != max(0, test.after) {
		t.Fatal("existing schema 6 row lost attempt evidence or its CI bound", got)
	}
	listed, err := s.PendingPublications(t.Context(), stateport.RecordFilter{Limit: 10})
	requireOK(t, err)
	if len(listed) != 1 || listed[0].Attempted != got.Attempted || listed[0].CIAfter != got.CIAfter {
		t.Fatal("list decoded existing publication differently", listed, got)
	}
	_, err = s.BindPublication(t.Context(), stateport.Action{Actor: "operator"}, id, "")
	requireOK(t, err)
	var stored bool
	requireOK(t, s.readers.QueryRowContext(t.Context(), `SELECT json_extract(value,'$.attempted') FROM publications WHERE id=?`, id).Scan(&stored))
	if stored != test.attempted {
		t.Fatal("write did not separate existing attempt evidence", stored)
	}
}
