//go:build linux

package main

import (
	"os"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/gitcache/gittest"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/service"
	"github.com/oberthci/oberth/internal/stateport"
	"github.com/oberthci/oberth/internal/stepmodel"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestComposedReleaseRerunRequiresFreshAncestry(t *testing.T) {
	f := composeFixture(t, composeBinary(t))
	commit := f.prepareCommit(t)
	f.prepareAdministrator(t)
	cancel, done := f.run(t)
	defer cancel()
	f.checkStarting(t, done)
	f.waitStarted(t, done)
	f.bindClient(t)
	f.releaseBuild(t, "refs/heads/tested", true)
	f.git.Git(t, f.git.Work, "tag", "v1.2.3", commit)
	f.git.Git(t, f.git.Work, "push", "oberth", "refs/tags/v1.2.3")
	run := f.waitMatchingRun(t, func(r stateport.Run) bool { return r.Ref == "refs/tags/v1.2.3" && r.Origin.WorkflowUID != "" })
	f.deliverSource(t, run)
	if err := f.owners.c.store.Finish(t.Context(), stateport.Action{Actor: "administrator"}, stateport.RunUpdate{ID: run.ID, Revision: run.Revision, Status: stepmodel.RunFailed, Reason: "build command failed"}); err != nil {
		t.Fatal(err)
	}
	who := f.joinedActor(t)
	who.Admin = true
	request := service.StartRequest{Request: model.CIRequest{ReceiveID: "fresh-release-rerun", Repo: gittest.Repository, Ref: run.Ref, ObjectSHA: run.ObjectSHA, CommitSHA: run.CommitSHA, Trigger: model.TriggerTag, Actor: who}, RerunOf: run.ID}
	f.git.Write(t, "later", []byte("still contains the release"))
	f.git.CommitFile(t, "advance default after release")
	f.git.Git(t, f.git.Work, "push", f.git.Bare, "HEAD:refs/heads/main")
	resolved, err := f.owners.ResolveStart(t.Context(), request)
	if err != nil || !resolved.Release || resolved.Original == nil {
		t.Fatal("fresh reachable release rerun was refused", err)
	}
	f.git.Git(t, f.git.Bare, "update-ref", "refs/heads/main", f.git.Commit)
	t.Run("rewritten default", func(t *testing.T) {
		f.checkReleaseRerunRefusal(t, request, "outside default history")
	})
	// The cache still contains the admitted head and original release facts.
	// A forge failure must not turn those into authority for another release.
	t.Run("forge unavailable", func(t *testing.T) {
		if err := os.Rename(f.git.Bare, f.git.Bare+".offline"); err != nil {
			t.Fatal(err)
		}
		f.checkReleaseRerunRefusal(t, request, "release default head is unavailable - Try again once the forge answers.")
	})
	f.checkReleaseRerunUnchanged(t, run)
}

func (f *composedFixture) checkReleaseRerunRefusal(t *testing.T, request service.StartRequest, reason string) {
	t.Helper()
	if _, err := f.owners.wiring.StartRun(t.Context(), request); err == nil || !strings.Contains(err.Error(), reason) {
		t.Fatal("release rerun reused retained ancestry", reason, err)
	}
}

func (f *composedFixture) checkReleaseRerunUnchanged(t *testing.T, run stateport.Run) {
	t.Helper()
	native, err := f.argo.ArgoprojV1alpha1().Workflows(f.config.PipelineNamespace).List(t.Context(), meta.ListOptions{})
	if err != nil || len(native.Items) != 2 {
		t.Fatal("refused release rerun created native work", len(native.Items), err)
	}
	rows, err := f.owners.c.store.Runs(t.Context(), stateport.RunFilter{Limit: 20})
	if err != nil || len(rows) != 2 {
		t.Fatal("refused release rerun created a run", len(rows), err)
	}
	retained, err := f.owners.c.store.Run(t.Context(), run.ID)
	if err != nil || retained.Admission.Digest != run.Admission.Digest {
		t.Fatal("release recheck changed retained admission", err)
	}
}
