//go:build linux

package main

import (
	"slices"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/gitcache/gittest"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/stateport"
)

func amendChainFixture(t *testing.T) (*composedFixture, string) {
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
	return f, base
}

func pushAmend(t *testing.T, f *composedFixture, message string, run bool) string {
	t.Helper()
	f.git.Git(t, f.git.Work, "commit", "--amend", "-m", message)
	sha := f.git.Git(t, f.git.Work, "rev-parse", "HEAD")
	options := []string{"--force"}
	if run {
		options = append(options, "-o", "oberth.run=1")
	}
	if output, err := f.pushResult(t, "refs/heads/topic", options...); err != nil {
		t.Fatal(output, err)
	}
	return sha
}

func TestRV2SyncAfterTwoAmends(t *testing.T) { checkAmendChain(t, false, false) }
func TestRV2RunAfterTwoAmends(t *testing.T)  { checkAmendChain(t, true, false) }
func TestAmendChainPreservesUnseenForgeCommit(t *testing.T) {
	for _, run := range []bool{false, true} {
		t.Run(map[bool]string{false: "sync", true: "run"}[run], func(t *testing.T) { checkAmendChain(t, run, true) })
	}
}

func checkAmendChain(t *testing.T, run, unseen bool) {
	t.Helper()
	f, base := amendChainFixture(t)
	first := pushAmend(t, f, "first amend", run)
	second := pushAmend(t, f, "second amend", run)
	expected := second
	if unseen {
		f.git.Git(t, f.git.Work, "checkout", "-B", "external", base)
		f.git.Write(t, "external", []byte("unseen forge commit"))
		expected = f.git.CommitFile(t, "external topic")
		f.git.Git(t, f.git.Work, "push", f.git.Bare, "HEAD:refs/heads/topic")
		setPushCacheAge(t, f, 16*time.Minute)
		if err := f.owners.cache.Fetch(t.Context(), gittest.Repository); err != nil {
			t.Fatal(err)
		}
	}
	if run {
		r := chainRun(t, f, second)
		f.sourcePod(t, r)
		f.finishNative(t, r)
	} else {
		call, err := f.owners.service.PrepareSync(t.Context(), apicontract.Caller{Actor: f.joinedActor(t)}, apicontract.SyncArgs{Repo: gittest.Repository, Branch: "topic", SHA: second})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := call(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	p := settledTopicPublication(t, f)
	if !slices.Equal(p.ReceivedHeads, []string{first, base}) {
		t.Fatal("receive chain was lost", p)
	}
	want := model.PublicationDelivered
	if unseen {
		want = model.PublicationAbandoned
	}
	if p.Status != want {
		t.Fatal("unexpected amend-chain publication", p)
	}
	if actual := f.git.Git(t, f.git.Bare, "rev-parse", "refs/heads/topic"); actual != expected {
		t.Fatal("unexpected forge head", actual, expected)
	}
}

func chainRun(t *testing.T, f *composedFixture, sha string) stateport.Run {
	t.Helper()
	deadline := time.NewTimer(15 * time.Second)
	defer deadline.Stop()
	for {
		changed := observeChanges(t, f.owners.c.store)
		rows, err := f.owners.c.store.Runs(t.Context(), stateport.RunFilter{Limit: 5})
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range rows {
			if r.ObjectSHA == sha && r.Origin.WorkflowUID != "" {
				run, err := f.owners.c.store.Run(t.Context(), r.ID)
				if err != nil {
					t.Fatal(err)
				}
				return run
			}
		}
		select {
		case <-changed:
		case <-deadline.C:
			t.Fatal("second amend run did not start", rows)
		}
	}
}
