//go:build linux

package main

import (
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/gitcache/gittest"
	"github.com/oberthci/oberth/internal/model"
)

func TestTopicSyncPublishesThePushersAmendedCommit(t *testing.T) {
	f := composeFixture(t, composeBinary(t))
	base := f.prepareCommit(t)
	f.git.Git(t, f.git.Work, "push", f.git.Bare, base+":refs/heads/topic")
	f.git.Git(t, f.git.Work, "commit", "--amend", "-m", "parked amended topic")
	amended := f.git.Git(t, f.git.Work, "rev-parse", "HEAD")
	f.prepareAdministrator(t)
	cancel, done := f.run(t)
	defer cancel()
	f.checkStarting(t, done)
	f.waitStarted(t, done)
	f.enrollJoined(t)
	f.bindClient(t)
	if output, err := f.pushResult(t, "refs/heads/topic", "--force"); err != nil {
		t.Fatal(output, err)
	}
	setPushCacheAge(t, f, 16*time.Minute)
	if err := f.owners.cache.Fetch(t.Context(), gittest.Repository); err != nil {
		t.Fatal(err)
	}
	call, err := f.owners.service.PrepareSync(t.Context(), apicontract.Caller{Actor: f.joinedActor(t)}, apicontract.SyncArgs{Repo: gittest.Repository, Branch: "topic", SHA: amended})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := call(t.Context()); err != nil {
		t.Fatal(err)
	}
	p := settledTopicPublication(t, f)
	if p.Status != model.PublicationDelivered || p.ReceivedSHA == nil || *p.ReceivedSHA != base {
		t.Fatal("parked rewrite lost its original receive evidence", p)
	}
	if head := f.git.Git(t, f.git.Bare, "rev-parse", "refs/heads/topic"); head != amended {
		t.Fatal("parked amended topic did not publish", head)
	}
}
