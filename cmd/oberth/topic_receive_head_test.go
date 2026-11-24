//go:build linux

package main

import (
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/gitcache/gittest"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/stateport"
)

func TestTopicRunKeepsReceiveHeadAcrossFetch(t *testing.T) {
	for _, change := range []string{"advanced", "deleted", "recreated", "force-pushed"} {
		t.Run(change, func(t *testing.T) { checkTopicRunReceiveHead(t, change) })
	}
}

func checkTopicRunReceiveHead(t *testing.T, change string) {
	f, base, run := receivedTopicRun(t, false)
	changeForgeTopic(t, f, base, change)
	before := f.git.Git(t, f.git.Bare, "for-each-ref", "--format=%(refname) %(objectname)")
	setPushCacheAge(t, f, 16*time.Minute)
	if err := f.owners.cache.Fetch(t.Context(), gittest.Repository); err != nil {
		t.Fatal(err)
	}
	f.sourcePod(t, run)
	f.finishNative(t, run)
	p := settledTopicPublication(t, f)
	if p.Status != model.PublicationAbandoned || p.ReceivedSHA == nil || *p.ReceivedSHA != base {
		t.Fatal("refresh changed the run's receive head", p)
	}
	if refs := f.git.Git(t, f.git.Bare, "for-each-ref", "--format=%(refname) %(objectname)"); refs != before {
		t.Fatal("refresh let CI change unseen forge history", refs)
	}
}

func changeForgeTopic(t *testing.T, f *composedFixture, base, change string) {
	t.Helper()
	if change == "deleted" || change == "recreated" {
		f.git.Git(t, f.git.Bare, "update-ref", "-d", "refs/heads/topic")
	}
	if change == "deleted" {
		return
	}
	f.git.Git(t, f.git.Work, "checkout", "-B", "external", base)
	f.git.Write(t, "external", []byte("unseen "+change+" change"))
	external := f.git.CommitFile(t, "external topic")
	if change != "advanced" {
		tree := f.git.Git(t, f.git.Work, "rev-parse", "HEAD^{tree}")
		external = f.git.Git(t, f.git.Work, "commit-tree", tree, "-m", "independent forge history")
	}
	f.git.Git(t, f.git.Work, "push", "--force", f.git.Bare, external+":refs/heads/topic")
}

func TestTopicRunExtendsAnUntrackedForgeHead(t *testing.T) {
	f, _, run := receivedTopicRun(t, true)
	f.sourcePod(t, run)
	f.finishNative(t, run)
	p := settledTopicPublication(t, f)
	if p.Status != model.PublicationDelivered {
		t.Fatal("child of an untracked forge head was refused", p)
	}
	if head := f.git.Git(t, f.git.Bare, "rev-parse", "refs/heads/topic"); head != *run.CommitSHA {
		t.Fatal("tested child did not reach the forge", head)
	}
}

func receivedTopicRun(t *testing.T, untrackedParent bool) (*composedFixture, string, stateport.Run) {
	t.Helper()
	f := composeFixture(t, composeBinary(t))
	base := f.prepareCommit(t)
	f.git.Git(t, f.git.Work, "push", f.git.Bare, base+":refs/heads/topic")
	f.prepareAdministrator(t)
	cancel, done := f.run(t)
	t.Cleanup(cancel)
	f.checkStarting(t, done)
	f.waitStarted(t, done)
	f.enrollJoined(t)
	f.bindClient(t)
	if untrackedParent {
		f.git.Write(t, "forge", []byte("pulled directly from the forge"))
		f.git.CommitFile(t, "untracked parent")
		f.git.Git(t, f.git.Work, "push", f.git.Bare, "HEAD:refs/heads/topic")
	}
	f.git.Write(t, "local", []byte("tested topic change"))
	f.git.CommitFile(t, "topic change")
	setPushCacheAge(t, f, 16*time.Minute)
	before := f.forge.sessions.Load()
	if output, err := f.pushResult(t, "refs/heads/topic", "-o", "oberth.run=1"); err != nil {
		t.Fatal(output, err)
	}
	if f.forge.sessions.Load() != before {
		t.Fatal("topic admission consulted the forge")
	}
	run := f.waitRun(t, func(r stateport.Run) bool { return r.Origin.WorkflowUID != "" })
	return f, base, run
}
