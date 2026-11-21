//go:build linux

// These tests exercise the Linux server and Git receive path.

package main

import (
	"database/sql"
	"github.com/oberthci/oberth/internal/gitcache"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/gitcache/gittest"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/service"
	"github.com/oberthci/oberth/internal/stateport"
	"github.com/oberthci/oberth/internal/stepmodel"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	ktest "k8s.io/client-go/testing"
)

func TestComposedRestartRecoversRunsPublicationsReplayAndRetention(t *testing.T) {
	t.Parallel()
	f := composeFixture(t, composeBinary(t))
	commit := f.prepareCommit(t)
	var unavailable atomic.Bool
	unavailable.Store(true)
	f.argo.PrependReactor("create", "workflows", func(ktest.Action) (bool, runtime.Object, error) {
		if unavailable.Load() {
			return true, nil, apierrors.NewServiceUnavailable("simulated interrupted native submission")
		}
		return false, nil, nil
	})
	cancel, done := f.run(t)
	f.checkStarting(t, done)
	f.waitStarted(t, done)
	f.bindClient(t)
	f.git.Git(t, f.git.Work, "push", "-o", "oberth.run=1", "oberth", "HEAD:refs/heads/pending")
	lost := f.waitRun(t, func(r stateport.Run) bool { return r.HasAdmission && r.Origin.WorkflowUID == "" })

	unavailable.Store(false)
	pending := f.leavePendingReceive(t)
	who := f.joinedActor(t)
	live, err := f.owners.wiring.StartRun(t.Context(), service.StartRequest{Request: model.CIRequest{ReceiveID: "live-before-restart", Repo: gittest.Repository, Ref: "refs/heads/main", ObjectSHA: commit, CommitSHA: &commit, Trigger: model.TriggerBranch, Actor: who}})
	if err != nil {
		t.Fatal("create live retention owner", err)
	}
	f.startPendingPublication(t, commit)
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("first server did not stop")
	}
	f.restartChannels(t)
	_, restarted := f.run(t)
	f.checkStarting(t, restarted)
	f.waitStarted(t, restarted)
	f.checkRecoveredRun(t, lost.ID)
	f.checkReplayedPush(t, pending.Event.ID)
	f.checkRecoveredPublication(t)
	retained, err := f.owners.c.store.Run(t.Context(), live.ID)
	if err != nil || terminalRun(retained.Status) || retained.Origin.WorkflowUID == "" {
		t.Fatal("restart lost the live native run", retained.RunSummary, err)
	}
	f.checkRetention(t, retained)
}
func (f *composedFixture) checkRecoveredRun(t *testing.T, id string) {
	t.Helper()
	recovered, err := f.owners.c.store.Run(t.Context(), id)
	if err != nil || recovered.Status != stepmodel.RunFailed {
		t.Fatal("startup did not settle unbound run", recovered.RunSummary, err)
	}
}
func (f *composedFixture) checkReplayedPush(t *testing.T, id string) {
	t.Helper()
	if replayed, err := f.owners.c.store.AcceptedRuns(t.Context(), id); err != nil || len(replayed) != 1 || replayed[0].Origin.WorkflowUID == "" {
		t.Fatal("pending push was not replayed into one native run", replayed, err)
	}
	if _, err := f.owners.cache.PendingReceive(t.Context(), gittest.Repository); !os.IsNotExist(err) {
		t.Fatal("startup did not acknowledge pending receive", err)
	}
}
func (f *composedFixture) checkRecoveredPublication(t *testing.T) {
	t.Helper()
	publication, err := f.owners.c.store.Publication(t.Context(), "restart-publication")
	if err != nil || publication.Status != model.PublicationDelivered || publication.Attempts != 1 {
		t.Fatal("startup did not settle durable publication by readback", publication, err)
	}
}

func (f *composedFixture) startPendingPublication(t *testing.T, commit string) {
	t.Helper()
	db := f.owners.c.store
	action := stateport.Action{Actor: "agent"}
	p := model.Publication{ID: "restart-publication", Repo: gittest.Repository, Ref: "refs/heads/main", ObjectSHA: commit, CommitSHA: &commit, Actor: action.Actor}
	if err := db.PreparePublication(t.Context(), action, stateport.PublicationIntent{Publication: p}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.StartPublicationAttempt(t.Context(), action, stateport.PublicationAttempt{ID: p.ID}); err != nil {
		t.Fatal(err)
	}
}
func (f *composedFixture) restartChannels(t *testing.T) {
	t.Helper()
	f.trace = &serverTrace{ready: make(chan struct{})}
	f.listeners = map[string]*memoryListener{}
	f.listed, f.allowList = make(chan struct{}), make(chan struct{})
	f.workflowWatch(t)
}
func (f *composedFixture) checkRetention(t *testing.T, r stateport.Run) {
	t.Helper()
	if err := f.owners.cache.GC(t.Context()); err != nil {
		t.Fatal("collect cache with live run", err)
	}
	path := filepath.Join(filepath.Dir(f.config.DatabaseFile), "git", "origin", "team", "widget.git")
	refs := f.git.Git(t, path, "for-each-ref", "--format=%(refname)", "refs/oberth/keep/"+r.ID+"/")
	if !strings.Contains(refs, "refs/oberth/keep/"+r.ID+"/") {
		t.Fatal("GC discarded a live source owner", r.ID, refs)
	}
}

func (f *composedFixture) leavePendingReceive(t *testing.T) gitcache.Reservation {
	t.Helper()
	db, err := sql.Open("sqlite", f.config.DatabaseFile)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err = db.ExecContext(t.Context(), "CREATE TRIGGER interrupt_receive BEFORE INSERT ON receives BEGIN SELECT RAISE(ABORT, 'test interrupted receive commit'); END"); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if _, err := db.ExecContext(t.Context(), "DROP TRIGGER interrupt_receive"); err != nil {
			t.Error(err)
		}
	}()
	_, _ = f.pushResult(t, "refs/heads/replay-after-restart", "-o", "oberth.run=1")
	pending, err := f.owners.cache.PendingReceive(t.Context(), gittest.Repository)
	if err != nil || pending.State != "ready" {
		t.Fatal("interrupted receive was not durable", pending, err)
	}
	if _, err = f.owners.c.store.AcceptedRuns(t.Context(), pending.Event.ID); err == nil {
		t.Fatal("interrupted receipt was already committed")
	}
	return pending
}
