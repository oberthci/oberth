package store

import (
	"errors"
	"github.com/oberthci/oberth/internal/model"
	"strings"
	"testing"
	"time"
)

func TestRepositoryAmbiguityListsQualifiedCandidates(t *testing.T) {
	now := time.Now()
	s := testStore(t, &now)
	var repos []model.Repository
	for _, name := range []string{"first", "second", "third"} {
		up, err := s.CreateUpstream(t.Context(), model.UpstreamSpec{Name: name, Kind: "ssh", BaseURL: "ssh://git@example.com/team"})
		if err != nil {
			t.Fatal(err)
		}
		repo, err := s.CreateRepository(t.Context(), model.RepositorySpec{Name: "widget", UpstreamID: up.ID, DefaultBranch: "main"})
		if err != nil {
			t.Fatal(err)
		}
		repos = append(repos, repo)
	}
	for _, input := range []string{"widget", "team/widget"} {
		_, err := s.RepositoryByName(t.Context(), input)
		if !errors.Is(err, ErrAmbiguous) {
			t.Fatalf("%q selected a repository: %v", input, err)
		}
		for i, name := range []string{"first", "second", "third"} {
			qualified := name + "/team/widget"
			if !strings.Contains(err.Error(), qualified) {
				t.Errorf("missing %s: %v", qualified, err)
			}
			got, lookupErr := s.RepositoryByName(t.Context(), qualified)
			if lookupErr != nil || got.ID != repos[i].ID {
				t.Fatalf("candidate cannot be used: %+v %v", got, lookupErr)
			}
		}
	}
}
