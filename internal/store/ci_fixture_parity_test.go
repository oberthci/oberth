package store_test

import (
	"testing"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/store"
	"github.com/oberthci/oberth/internal/storetest"
)

func TestCIFixtureDoesNotStealIssueLock(t *testing.T) {
	s, err := storetest.Open(t.Context(), t.TempDir()+"/oberth.sqlite", store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	u, err := storetest.CreateUpstream(s, t.Context(), model.UpstreamSpec{Name: "origin", Kind: "ssh", BaseURL: "ssh://git@example.invalid/org"})
	if err != nil {
		t.Fatal(err)
	}
	repo, err := s.CreateRepository(t.Context(), model.RepositorySpec{Name: "repo", UpstreamID: u.ID, DefaultBranch: "main"})
	if err != nil {
		t.Fatal(err)
	}
	issue, err := storetest.UpsertCIIssue(s, t.Context(), "owner", repo.ID, "feature", "red", "first")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.AcquireIssueLock(t.Context(), issue.ID, "owner"); err != nil {
		t.Fatal(err)
	}
	if _, err = storetest.UpsertCIIssue(s, t.Context(), "other", repo.ID, "feature", "red again", "second"); err != nil {
		t.Fatal(err)
	}
	if _, err = s.RenewIssueLock(t.Context(), issue.ID, "owner"); err != nil {
		t.Fatalf("fixture stole lock unlike production projection: %v", err)
	}
}
