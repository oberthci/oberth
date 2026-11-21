package store

import (
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/publicerr"
	"github.com/oberthci/oberth/internal/stateport"
	"github.com/oberthci/oberth/internal/stepmodel"
)

func TestPublicationFiveAttemptsAndNoRepeatedPush(t *testing.T) {
	t.Parallel()
	options := testOptions()
	base := options.Clock()
	var offset atomic.Int64
	options.Clock = func() time.Time { return base.Add(time.Duration(offset.Load())) }
	s, err := Create(t.Context(), filepath.Join(t.TempDir(), "state.sqlite"), options)
	requireOK(t, err)
	defer func() { requireOK(t, s.Close()) }()
	r, _ := accepted(t, s, "five-attempts")
	requireOK(t, s.Finish(t.Context(), stateport.Action{Actor: "observer"}, stateport.RunUpdate{ID: r.ID, Revision: r.Revision, Status: stepmodel.RunPassed}))
	prior := strings.Repeat("b", 40)
	p := model.Publication{ID: "push-one", RunID: r.ID, Repo: r.Repo, Ref: r.Ref, ObjectSHA: r.ObjectSHA, PreviousSHA: &prior, Actor: "operator"}
	requireOK(t, s.PreparePublication(t.Context(), stateport.Action{Actor: "operator"}, stateport.PublicationIntent{Publication: p}))
	for attempt := 1; attempt <= 5; attempt++ {
		started, err := s.StartPublicationAttempt(t.Context(), stateport.Action{Actor: "operator"}, stateport.PublicationAttempt{ID: p.ID, ExpectedAttempts: attempt - 1})
		requireOK(t, err)
		if started.Attempts != attempt {
			t.Fatal("attempt CAS")
		}
		_, err = s.StartPublicationAttempt(t.Context(), stateport.Action{Actor: "operator"}, stateport.PublicationAttempt{ID: p.ID, ExpectedAttempts: attempt})
		requireCode(t, err, publicerr.Conflict)
		requireOK(t, s.SettlePublication(t.Context(), stateport.Action{Actor: "operator"}, stateport.PublicationResult{ID: p.ID, Attempt: attempt, Retryable: true}))
		current, err := s.Publication(t.Context(), p.ID)
		requireOK(t, err)
		if attempt < 5 {
			if current.RetryAt == nil {
				t.Fatal("retry time not persisted")
			}
			_, err = s.StartPublicationAttempt(t.Context(), stateport.Action{Actor: "operator"}, stateport.PublicationAttempt{ID: p.ID, ExpectedAttempts: attempt})
			requireCode(t, err, publicerr.Conflict)
			offset.Store(int64(current.RetryAt.Sub(base)))
		} else if current.Status != model.PublicationFailed {
			t.Fatal("attempt budget did not settle")
		}
	}
	_, err = s.StartPublicationAttempt(t.Context(), stateport.Action{Actor: "operator"}, stateport.PublicationAttempt{ID: p.ID, ExpectedAttempts: 5})
	requireCode(t, err, publicerr.Conflict)
}
