//go:build linux

package main

import (
	"k8s.io/utils/clock"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/stateport"
	"github.com/oberthci/oberth/internal/stepmodel"
)

func stalePushCache(t *testing.T, f *composedFixture) {
	t.Helper()
	setPushCacheAge(t, f, time.Minute)
}

func setPushCacheAge(t *testing.T, f *composedFixture, age time.Duration) {
	t.Helper()
	marker := filepath.Join(filepath.Dir(f.config.DatabaseFile), "git", "origin", "team", "widget.git", "oberth-refreshed")
	data, err := os.ReadFile(marker)
	if err != nil {
		t.Fatal(err)
	}
	_, identity, _ := strings.Cut(string(data), "\n")
	if err := os.WriteFile(marker, []byte((clock.RealClock{}).Now().Add(-age).Format(time.RFC3339Nano)+"\n"+identity), 0600); err != nil {
		t.Fatal(err)
	}
}

func TestTopicPushSkipsStaleForgeRefreshAndPublicationChecksForge(t *testing.T) {
	f := composeFixture(t, composeBinary(t))
	f.prepareCommit(t)
	f.prepareAdministrator(t)
	cancel, done := f.run(t)
	defer cancel()
	f.checkStarting(t, done)
	f.waitStarted(t, done)
	f.enrollJoined(t)
	f.bindClient(t)
	stalePushCache(t, f)
	before := f.forge.sessions.Load()
	start := (clock.RealClock{}).Now()
	if output, err := f.pushResult(t, "refs/heads/topic", "-o", "oberth.run=1"); err != nil {
		t.Fatal(output, err)
	}
	delta := f.forge.sessions.Load() - before
	t.Logf("topic push with 60s-old cache: %v; forge sessions=%d", (clock.RealClock{}).Since(start), delta)
	if delta != 0 {
		t.Fatal("topic push refreshed the forge", delta)
	}
	run := f.waitRun(t, func(r stateport.Run) bool { return r.Origin.WorkflowUID != "" })
	if got := f.git.Git(t, f.git.Bare, "for-each-ref", "--format=%(objectname)", "refs/heads/topic"); got != "" {
		t.Fatal("topic published before CI passed", got)
	}
	// A change outside Oberth after the receive must survive publication.
	f.git.Write(t, "external", []byte("external forge change"))
	external := f.git.CommitFile(t, "external")
	f.git.Git(t, f.git.Work, "push", f.git.Bare, "HEAD:refs/heads/topic")
	f.sourcePod(t, run)
	f.finishNative(t, run)
	f.waitRun(t, func(r stateport.Run) bool { return r.Status == stepmodel.RunPassed })
	deadline := time.After(10 * time.Second)
	for {
		changed := observeChanges(t, f.owners.c.store)
		rows, err := f.owners.c.store.Publications(t.Context(), stateport.RecordFilter{Limit: 20})
		if err != nil {
			t.Fatal(err)
		}
		if len(rows) != 0 && rows[0].Status != "pending" {
			break
		}
		select {
		case <-changed:
		case <-deadline:
			t.Fatal("publication did not settle")
		}
	}
	if f.forge.sessions.Load() <= before {
		t.Fatal("publication skipped its own forge check")
	}
	if actual := f.git.Git(t, f.git.Bare, "rev-parse", "refs/heads/topic"); actual != external {
		t.Fatal("publication overwrote the external topic", actual)
	}
}

func TestDefaultPushStillReadsLiveForgeHead(t *testing.T) {
	f := composeFixture(t, composeBinary(t))
	f.prepareCommit(t)
	f.prepareAdministrator(t)
	cancel, done := f.run(t)
	defer cancel()
	f.checkStarting(t, done)
	f.waitStarted(t, done)
	f.bindOperatorPush(t)
	stalePushCache(t, f)
	f.git.Write(t, "change", []byte("default change"))
	commit := f.git.CommitFile(t, "default")
	before := f.forge.sessions.Load()
	if output, err := f.pushResult(t, "refs/heads/main"); err != nil {
		t.Fatal(output, err)
	}
	if delta := f.forge.sessions.Load() - before; delta == 0 {
		t.Fatal("default push did not read the live forge head")
	} else {
		t.Logf("default push forge sessions=%d", delta)
	}
	if actual := f.git.Git(t, f.git.Bare, "rev-parse", "refs/heads/main"); actual == commit {
		t.Fatal("default push published before passing CI")
	}
	f.waitRun(t, func(r stateport.Run) bool { return r.Ref == "refs/heads/main" && r.Origin.WorkflowUID != "" })
}

func TestDefaultPushRefusesNewHeadOutsideLiveForgeHistory(t *testing.T) {
	f := composeFixture(t, composeBinary(t))
	seed := f.prepareCommit(t)
	f.prepareAdministrator(t)
	cancel, done := f.run(t)
	defer cancel()
	f.checkStarting(t, done)
	f.waitStarted(t, done)
	f.bindOperatorPush(t)
	f.git.Write(t, "external", []byte("external default"))
	external := f.git.CommitFile(t, "forge default")
	f.git.Git(t, f.git.Work, "push", f.git.Bare, "HEAD:refs/heads/main")
	f.git.Git(t, f.git.Work, "checkout", "-B", "local", seed)
	f.git.Write(t, "local", []byte("sibling default"))
	f.git.CommitFile(t, "local sibling")
	if output, err := f.pushResult(t, "refs/heads/main"); err == nil || !strings.Contains(output, "push is not a fast-forward") {
		t.Fatal("default push bypassed the live head", output, err)
	}
	if actual := f.git.Git(t, f.git.Bare, "rev-parse", "refs/heads/main"); actual != external {
		t.Fatal("default push overwrote forge history", actual)
	}
}
