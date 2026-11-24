//go:build linux

package main

import (
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/gitcache/gittest"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/stateport"
)

func TestTopicRunPublishesThePushersAmendedCommit(t *testing.T) {
	f := composeFixture(t, composeBinary(t))
	base := f.prepareCommit(t)
	f.git.Git(t, f.git.Work, "push", f.git.Bare, base+":refs/heads/topic")
	f.git.Git(t, f.git.Work, "commit", "--amend", "-m", "amended topic")
	amended := f.git.Git(t, f.git.Work, "rev-parse", "HEAD")
	f.prepareAdministrator(t)
	cancel, done := f.run(t)
	defer cancel()
	f.checkStarting(t, done)
	f.waitStarted(t, done)
	f.enrollJoined(t)
	f.bindClient(t)
	if output, err := f.pushResult(t, "refs/heads/topic", "--force", "-o", "oberth.run=1"); err != nil {
		t.Fatal(output, err)
	}
	run := f.waitRun(t, func(r stateport.Run) bool { return r.Origin.WorkflowUID != "" })
	f.sourcePod(t, run)
	f.finishNative(t, run)
	publication := settledTopicPublication(t, f)
	if publication.Status != model.PublicationDelivered || publication.PreviousSHA == nil || *publication.PreviousSHA != base {
		t.Fatal("a deliberate topic rewrite did not publish with its observed lease", publication)
	}
	if actual := f.git.Git(t, f.git.Bare, "rev-parse", "refs/heads/topic"); actual != amended {
		t.Fatal("amended topic was not published", actual)
	}
}

func TestTopicSyncPreservesTheUnseenForgeCommit(t *testing.T) {
	for _, refresh := range []bool{false, true} {
		t.Run(map[bool]string{false: "without fetch", true: "after fetch"}[refresh], func(t *testing.T) { checkTopicSyncUnseenCommit(t, refresh) })
	}
}

func checkTopicSyncUnseenCommit(t *testing.T, refresh bool) {
	f := composeFixture(t, composeBinary(t))
	base := f.prepareCommit(t)
	f.git.Git(t, f.git.Work, "push", f.git.Bare, base+":refs/heads/topic")
	f.git.Write(t, "local", []byte("cached local change"))
	local := f.git.CommitFile(t, "local topic")
	f.prepareAdministrator(t)
	cancel, done := f.run(t)
	defer cancel()
	f.checkStarting(t, done)
	f.waitStarted(t, done)
	f.enrollJoined(t)
	f.bindClient(t)
	setPushCacheAge(t, f, 16*time.Minute)
	f.git.Git(t, f.git.Work, "checkout", "-B", "external", base)
	f.git.Write(t, "external", []byte("unseen forge change"))
	external := f.git.CommitFile(t, "external topic")
	f.git.Git(t, f.git.Work, "push", f.git.Bare, "HEAD:refs/heads/topic")
	f.git.Git(t, f.git.Work, "checkout", "-B", "local", local)
	before := f.forge.sessions.Load()
	if output, err := f.pushResult(t, "refs/heads/topic"); err != nil {
		t.Fatal(output, err)
	}
	if f.forge.sessions.Load() != before {
		t.Fatal("cached topic consulted the forge during admission")
	}
	if refresh {
		setPushCacheAge(t, f, 16*time.Minute)
		if err := f.owners.cache.Fetch(t.Context(), gittest.Repository); err != nil {
			t.Fatal(err)
		}
	}
	call, err := f.owners.service.PrepareSync(t.Context(), apicontract.Caller{Actor: f.joinedActor(t)}, apicontract.SyncArgs{Repo: gittest.Repository, Branch: "topic", SHA: local})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := call(t.Context()); err != nil {
		t.Fatal(err)
	}
	publication := settledTopicPublication(t, f)
	if publication.Status == model.PublicationDelivered || publication.ReceivedSHA == nil || *publication.ReceivedSHA != base {
		t.Fatal("sync lost its original receive head", publication)
	}
	if actual := f.git.Git(t, f.git.Bare, "rev-parse", "refs/heads/topic"); actual != external {
		t.Fatal("sync overwrote the unseen forge commit", actual)
	}
}
