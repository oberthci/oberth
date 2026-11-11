package store

import (
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"
)

func TestOrdinaryTagReceiveKeepsObjectAndCommitWithoutRelease(t *testing.T) {
	now := time.Now()
	s := testStore(t, &now)
	repo := createRepo(t, s)
	object, commit := strings.Repeat("e", 40), strings.Repeat("a", 40)
	event := model.ReceiveEventSpec{ID: "ordinary-tag", Actor: "admin", RepoID: repo.ID, RefKind: model.RefTag, Ref: "deploy-1", ObjectSHA: object, CommitSHA: commit, Outcome: "queued"}
	spec := model.RunSpec{RepoID: repo.ID, RefKind: model.RefTag, Ref: event.Ref, SHA: object, TestedSHA: commit, Actor: event.Actor, Trigger: "tag"}
	queued, err := s.EnqueueReceiveEvent(t.Context(), event, spec)
	if err != nil {
		t.Fatal(err)
	}
	if queued.Release || queued.Credentialed || queued.SHA != object || queued.TestedSHA != commit || queued.RefKind != model.RefTag {
		t.Fatalf("tag authority/provenance: %+v", queued.Run)
	}
	duplicate, err := s.EnqueueReceiveEvent(t.Context(), event, spec)
	if err != nil || !duplicate.Duplicate || duplicate.ID != queued.ID {
		t.Fatal("ordinary tag receive was not idempotent")
	}
	spec.TestedSHA = strings.Repeat("b", 40)
	if _, err := s.EnqueueReceiveEvent(t.Context(), event, spec); err == nil {
		t.Fatal("accepted another commit under the same tag receipt")
	}
}
