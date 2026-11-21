package store

import (
	"testing"

	"github.com/oberthci/oberth/internal/stateport"
)

func atomicRepository(t *testing.T, s *Store) stateport.Repository {
	t.Helper()
	requireOK(t, s.RegisterUpstream(t.Context(), stateport.Action{Actor: "operator"}, stateport.Upstream{Name: "origin", Org: "team", Kind: "git", BaseURL: "https://oberth.ci", KeyName: "source"}))
	r, err := s.RegisterRepository(t.Context(), stateport.Action{Actor: "operator"}, "origin/team/repo")
	requireOK(t, err)
	return r
}

func TestAtomicRepositoryFacts(t *testing.T) {
	t.Parallel()
	checkAtomicFacts(t, []atomicFact{
		{"upstream.register", func(t *testing.T, s *Store) func(stateport.Action) error {
			return func(a stateport.Action) error {
				return s.RegisterUpstream(t.Context(), a, stateport.Upstream{Name: "origin", Org: "team", Kind: "git", BaseURL: "https://oberth.ci", KeyName: "source"})
			}
		}},
		{"upstream.remove", func(t *testing.T, s *Store) func(stateport.Action) error {
			requireOK(t, s.RegisterUpstream(t.Context(), stateport.Action{Actor: "operator"}, stateport.Upstream{Name: "origin", Org: "team", Kind: "git", BaseURL: "https://oberth.ci", KeyName: "source"}))
			return func(a stateport.Action) error { return s.RemoveUpstream(t.Context(), a, "origin") }
		}},
		{"repository.register", func(t *testing.T, s *Store) func(stateport.Action) error {
			requireOK(t, s.RegisterUpstream(t.Context(), stateport.Action{Actor: "operator"}, stateport.Upstream{Name: "origin", Org: "team", Kind: "git", BaseURL: "https://oberth.ci", KeyName: "source"}))
			return func(a stateport.Action) error {
				_, err := s.RegisterRepository(t.Context(), a, "origin/team/repo")
				return err
			}
		}},
		{"repository.archive", func(t *testing.T, s *Store) func(stateport.Action) error {
			r := atomicRepository(t, s)
			return func(a stateport.Action) error { return s.ArchiveRepository(t.Context(), a, r.ID) }
		}},
		{"repository.unarchive", func(t *testing.T, s *Store) func(stateport.Action) error {
			r := atomicRepository(t, s)
			requireOK(t, s.ArchiveRepository(t.Context(), stateport.Action{Actor: "operator"}, r.ID))
			return func(a stateport.Action) error { return s.UnarchiveRepository(t.Context(), a, r.ID) }
		}},
		{"repository.remove", func(t *testing.T, s *Store) func(stateport.Action) error {
			r := atomicRepository(t, s)
			return func(a stateport.Action) error { return s.RemoveRepository(t.Context(), a, r.ID) }
		}},
	})
}
