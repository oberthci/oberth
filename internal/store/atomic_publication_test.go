package store

import (
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/stateport"
	"github.com/oberthci/oberth/internal/stepmodel"
)

func publicationIntent(r stateport.Run) stateport.PublicationIntent {
	return stateport.PublicationIntent{Publication: model.Publication{ID: "publication", RunID: r.ID, Repo: r.Repo, Ref: r.Ref, ObjectSHA: r.ObjectSHA, PreviousSHA: new(strings.Repeat("b", 40)), Actor: "operator"}}
}

func publicationFixture(t *testing.T, s *Store) model.Publication {
	t.Helper()
	r, _ := accepted(t, s, "run")
	requireOK(t, s.Finish(t.Context(), stateport.Action{Actor: "operator"}, stateport.RunUpdate{ID: r.ID, Revision: r.Revision, Status: stepmodel.RunPassed}))
	input := publicationIntent(r)
	requireOK(t, s.PreparePublication(t.Context(), stateport.Action{Actor: "operator"}, input))
	return input.Publication
}

func TestAtomicPublicationFacts(t *testing.T) {
	t.Parallel()
	checkAtomicFacts(t, []atomicFact{
		{"publication.prepare", func(t *testing.T, s *Store) func(stateport.Action) error {
			r, _ := accepted(t, s, "run")
			requireOK(t, s.Finish(t.Context(), stateport.Action{Actor: "operator"}, stateport.RunUpdate{ID: r.ID, Revision: r.Revision, Status: stepmodel.RunPassed}))
			intent := publicationIntent(r)
			return func(a stateport.Action) error { return s.PreparePublication(t.Context(), a, intent) }
		}},
		{"publication.attempt", func(t *testing.T, s *Store) func(stateport.Action) error {
			p := publicationFixture(t, s)
			return func(a stateport.Action) error {
				_, err := s.StartPublicationAttempt(t.Context(), a, stateport.PublicationAttempt{ID: p.ID})
				return err
			}
		}},
		{"publication.settle", func(t *testing.T, s *Store) func(stateport.Action) error {
			p := publicationFixture(t, s)
			_, err := s.StartPublicationAttempt(t.Context(), stateport.Action{Actor: "operator"}, stateport.PublicationAttempt{ID: p.ID})
			requireOK(t, err)
			return func(a stateport.Action) error {
				return s.SettlePublication(t.Context(), a, stateport.PublicationResult{ID: p.ID, Attempt: 1, Delivered: true})
			}
		}},
		{"publication.abandon", func(t *testing.T, s *Store) func(stateport.Action) error {
			p := publicationFixture(t, s)
			return func(a stateport.Action) error { return s.AbandonPublication(t.Context(), a, p.ID) }
		}},
		{"publication.retry", func(t *testing.T, s *Store) func(stateport.Action) error {
			p := publicationFixture(t, s)
			_, err := s.StartPublicationAttempt(t.Context(), stateport.Action{Actor: "operator"}, stateport.PublicationAttempt{ID: p.ID})
			requireOK(t, err)
			requireOK(t, s.SettlePublication(t.Context(), stateport.Action{Actor: "operator"}, stateport.PublicationResult{ID: p.ID, Attempt: 1}))
			return func(a stateport.Action) error {
				return s.RetryPublication(t.Context(), a, stateport.PublicationAttempt{ID: p.ID, ExpectedAttempts: 1})
			}
		}},
	})
}

func TestAtomicFinishIntent(t *testing.T) {
	t.Parallel()
	checkAtomicFacts(t, []atomicFact{
		{"run.finish_with_intent", func(t *testing.T, s *Store) func(stateport.Action) error {
			r, _ := accepted(t, s, "run")
			p := publicationIntent(r)
			return func(a stateport.Action) error {
				return s.Finish(t.Context(), a, stateport.RunUpdate{ID: r.ID, Revision: r.Revision, Status: stepmodel.RunPassed, Publication: &p})
			}
		}},
	})
}
