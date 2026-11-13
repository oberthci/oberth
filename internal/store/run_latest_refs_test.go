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
	old := queuedRunForTest(t, s, repo.ID, "feature/a")
	middle := queuedRunForTest(t, s, repo.ID, "feature/b")
	newest := queuedRunForTest(t, s, repo.ID, "feature/a")
	// Terminal heads page by finish time; a queued head is never paged out.
	for _, id := range []string{middle.ID, newest.ID} {
		now = now.Add(time.Minute)
		if _, err := s.FinishRun(t.Context(), id, model.RunResult{Status: model.RunFailed}); err != nil {
			t.Fatal(err)
		}
	}
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

// ref_glob with latest (#1524): one newest run per matching ref, with
// path.Match semantics (* never crosses a slash), under the caller's scope.
func TestLatestRefHeadsFilterByRefGlob(t *testing.T) {
	now := time.Now()
	s := testStore(t, &now)
	repo := createRepo(t, s)
	queuedRunForTest(t, s, repo.ID, "land/a")
	newestA := queuedRunForTest(t, s, repo.ID, "land/a")
	landB := queuedRunForTest(t, s, repo.ID, "land/b")
	queuedRunForTest(t, s, repo.ID, "land/nested/c")
	queuedRunForTest(t, s, repo.ID, "feature/land")
	heads, err := s.ListRecentRuns(t.Context(), model.RunListFilter{LatestPerRef: true, RefGlob: "land/*", Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, head := range heads {
		got[head.Ref] = head.ID
	}
	if len(got) != 2 || got["land/a"] != newestA.ID || got["land/b"] != landB.ID {
		t.Fatalf("latest land/* heads = %v", got)
	}
	flat, err := s.ListRecentRuns(t.Context(), model.RunListFilter{RefGlob: "land/*", Limit: 100})
	if err != nil || len(flat) != 3 {
		t.Fatalf("flat land/* = %d %v", len(flat), err)
	}
	hidden, err := s.ListRecentRuns(t.Context(), model.RunListFilter{LatestPerRef: true, RefGlob: "land/*", RepoIDs: []int64{}, Limit: 100})
	if err != nil || len(hidden) != 0 {
		t.Fatalf("scope leaked: %v %v", hidden, err)
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
	source := queuedRunForTest(t, s, repo.ID, "kept")
	if _, err := s.db.Exec(publicationRetryDowngradeSQL + `DROP TABLE step_resource_usage; DROP INDEX runs_latest_ref_idx; DELETE FROM schema_migrations WHERE version>=35`); err != nil {
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
