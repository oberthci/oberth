package store

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"
)

func TestLatestRefHeadsCountAllAttemptsAndDoNotResurrectOnOlderPages(t *testing.T) {
	now := time.Now()
	s := testStore(t, &now)
	repo := createRepo(t, s)
	old := autoRetrySource(t, s, repo.ID, "feature/a")
	middle := autoRetrySource(t, s, repo.ID, "feature/b")
	newest := autoRetrySource(t, s, repo.ID, "feature/a")
	heads, err := s.ListRecentRuns(t.Context(), model.RunListFilter{LatestPerRef: true, Limit: 1})
	if err != nil || len(heads) != 1 || heads[0].ID != newest.ID || heads[0].EarlierAttempts != 1 {
		t.Fatal("wrong latest ref", heads, err)
	}
	heads, err = s.ListRecentRuns(t.Context(), model.RunListFilter{LatestPerRef: true, Limit: 1, Before: newest.QueueSequence})
	if err != nil || len(heads) != 1 || heads[0].ID != middle.ID {
		t.Fatal("wrong second page", heads, err)
	}
	heads, err = s.ListRecentRuns(t.Context(), model.RunListFilter{LatestPerRef: true, Limit: 1, Before: middle.QueueSequence})
	if err != nil || len(heads) != 0 {
		t.Fatal("older page resurrected old ref attempt", old, heads, err)
	}
	// Branch/tag with the same spelling are separate groups.
	tag, err := s.EnqueueRun(t.Context(), model.RunSpec{RepoID: repo.ID, RefKind: model.RefTag, Ref: newest.Ref, SHA: strings.Repeat("b", 40), Actor: "pusher", Trigger: "tag"})
	if err != nil {
		t.Fatal(err)
	}
	heads, err = s.ListRecentRuns(t.Context(), model.RunListFilter{LatestPerRef: true, Ref: newest.Ref, RepoIDs: []int64{repo.ID}, Limit: 10})
	if err != nil || len(heads) != 2 || heads[0].ID != tag.ID {
		t.Fatal("ref kind grouping failed", heads, err)
	}
	hidden, err := s.ListRecentRuns(t.Context(), model.RunListFilter{LatestPerRef: true, RepoIDs: []int64{}, Limit: 10})
	if err != nil || len(hidden) != 0 {
		t.Fatal("scope leaked heads/counts", hidden, err)
	}
}

func TestLatestRefHeadsRetainGlobalQueuePosition(t *testing.T) {
	now := time.Now()
	s := testStore(t, &now)
	repo := createRepo(t, s)
	for _, ref := range []string{"a", "b", "a", "c"} {
		autoRetrySource(t, s, repo.ID, ref)
	}
	flat, err := s.ListRecentRuns(t.Context(), model.RunListFilter{Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	positions := map[string]int64{}
	for _, r := range flat {
		positions[r.ID] = r.QueuePosition
	}
	heads, err := s.ListRecentRuns(t.Context(), model.RunListFilter{LatestPerRef: true, Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range heads {
		if r.Status == model.RunQueued && (r.QueuePosition == 0 || r.QueuePosition != positions[r.ID]) {
			t.Fatal("latest-ref head lost queue rank", r.ID, r.QueuePosition, positions[r.ID])
		}
	}
}

func TestSchemaV34LatestRefIndexUpgrade(t *testing.T) {
	now := time.Now()
	path := filepath.Join(t.TempDir(), "upgrade.db")
	s, err := Open(t.Context(), path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	repo := createRepo(t, s)
	source := autoRetrySource(t, s, repo.ID, "kept")
	if _, err := s.db.Exec(`DROP INDEX runs_latest_ref_idx; DELETE FROM schema_migrations WHERE version=35`); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(t.Context(), path, Options{Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	heads, err := s.ListRecentRuns(t.Context(), model.RunListFilter{LatestPerRef: true, Limit: 1})
	if err != nil || len(heads) != 1 || heads[0].ID != source.ID {
		t.Fatal("upgrade lost retained runs", heads, err)
	}
	var versions int
	if err := s.db.QueryRow(`SELECT count(*) FROM schema_migrations WHERE version=35`).Scan(&versions); err != nil || versions != 1 {
		t.Fatal("index migration not recorded once", versions, err)
	}
}
