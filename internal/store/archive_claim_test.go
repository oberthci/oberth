package store

import (
	"errors"
	"testing"
	"time"
)

func TestArchivedRepositoryRefusesNewRunHistory(t *testing.T) {
	now := time.Now()
	s := testStore(t, &now)
	repo := createRepo(t, s)
	if _, err := s.SetRepositoryArchived(t.Context(), "admin", repo.Name, true); err != nil {
		t.Fatal(err)
	}
	if _, err := s.EnqueueRun(t.Context(), testRunSpec(repo.ID, "main", projectionSourceSHA)); !errors.Is(err, ErrRepositoryArchived) {
		t.Fatalf("archived run accepted: %v", err)
	}
}
