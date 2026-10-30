package store

import (
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
