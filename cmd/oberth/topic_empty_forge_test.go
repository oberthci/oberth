//go:build linux

package main

import (
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/stateport"
	"github.com/oberthci/oberth/internal/stepmodel"
)

func TestCachedTopicCannotPublishTheFirstRefOfAnEmptyForge(t *testing.T) {
	f := composeFixture(t, composeBinary(t))
	f.prepareCommit(t)
	f.prepareAdministrator(t)
	cancel, done := f.run(t)
	defer cancel()
	f.checkStarting(t, done)
	f.waitStarted(t, done)
	f.enrollJoined(t)
	f.bindClient(t)
	setPushCacheAge(t, f, 0)
	f.git.Git(t, f.git.Bare, "update-ref", "-d", "refs/heads/main")
	before := f.forge.sessions.Load()
	if output, err := f.pushResult(t, "refs/heads/topic", "-o", "oberth.run=1"); err != nil {
		t.Fatal(output, err)
	}
	if delta := f.forge.sessions.Load() - before; delta != 0 {
		t.Fatal("topic consulted the empty forge during admission", delta)
	}
	run := f.waitRun(t, func(r stateport.Run) bool { return r.Origin.WorkflowUID != "" })
	f.sourcePod(t, run)
	f.finishNative(t, run)
	f.waitRun(t, func(r stateport.Run) bool { return r.Status == stepmodel.RunPassed })
	publication := settledTopicPublication(t, f)
	if publication.Status != model.PublicationFailed {
		t.Fatal("topic bootstrapped an empty forge", publication)
	}
	if refs := f.git.Git(t, f.git.Bare, "for-each-ref", "--format=%(refname)"); refs != "" {
		t.Fatal("publication ended the empty-forge state", refs)
	}
}

func settledTopicPublication(t *testing.T, f *composedFixture) model.Publication {
	t.Helper()
	deadline := time.After(10 * time.Second)
	for {
		changed := f.owners.c.store.Changes()
		rows, err := f.owners.c.store.Publications(t.Context(), stateport.RecordFilter{Limit: 20})
		if err != nil {
			t.Fatal(err)
		}
		if len(rows) == 1 && rows[0].Status != model.PublicationPending {
			return rows[0]
		}
		select {
		case <-changed:
		case <-deadline:
			t.Fatal("publication did not settle")
		}
	}
}
