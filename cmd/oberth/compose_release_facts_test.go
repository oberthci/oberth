//go:build linux

// These tests exercise the Linux server and Git receive path.

package main

import (
	"reflect"
	"strings"
	"testing"
	"time"

	wf "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	"github.com/oberthci/oberth/internal/admissioncontract"
	"github.com/oberthci/oberth/internal/gitcache/gittest"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/service"
	"github.com/oberthci/oberth/internal/stateport"
	"github.com/oberthci/oberth/internal/stepmodel"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestComposedReleaseFactsRequirePassedBuild(t *testing.T) {
	for _, kind := range []string{"lightweight", "annotated", "failed"} {
		t.Run(kind, func(t *testing.T) {
			checkComposedReleaseFacts(t, kind)
		})
	}
}

func (f *composedFixture) releaseBuild(t *testing.T, ref string, passed bool) stateport.Run {
	t.Helper()
	f.git.Git(t, f.git.Work, "push", "-o", "oberth.run=1", "oberth", "HEAD:"+ref)
	run := f.waitMatchingRun(t, func(r stateport.Run) bool { return r.Ref == ref && r.Origin.WorkflowUID != "" })
	f.deliverSource(t, run)
	if passed {
		f.finishNative(t, run)
	} else {
		client := f.argo.ArgoprojV1alpha1().Workflows(f.config.PipelineNamespace)
		w, err := client.Get(t.Context(), run.Origin.WorkflowName, meta.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		w.Status.Phase, w.Status.Message = wf.WorkflowFailed, "build command failed"
		w.Status.FinishedAt = meta.NewTime(f.clock.Now())
		if _, err := client.Update(t.Context(), w, meta.UpdateOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	status := stepmodel.RunFailed
	if passed {
		status = stepmodel.RunPassed
	}
	return f.waitMatchingRun(t, func(r stateport.Run) bool { return r.ID == run.ID && r.Status == status })
}

func (f *composedFixture) waitMatchingRun(t *testing.T, ready func(stateport.Run) bool) stateport.Run {
	t.Helper()
	deadline := time.NewTimer(10 * time.Second)
	defer deadline.Stop()
	for {
		changes := observeChanges(t, f.owners.c.store)
		rows, err := f.owners.c.store.Runs(t.Context(), stateport.RunFilter{Limit: 20})
		if err != nil {
			t.Fatal(err)
		}
		for _, row := range rows {
			if ready(row) {
				run, err := f.owners.c.store.Run(t.Context(), row.ID)
				if err != nil {
					t.Fatal(err)
				}
				return run
			}
		}
		select {
		case <-changes:
		case <-deadline.C:
			t.Fatalf("run did not reach expected state: %+v", rows)
		}
	}
}

func (f *composedFixture) assertReleaseParameters(t *testing.T, run stateport.Run, want map[string]string) {
	t.Helper()
	native, err := f.argo.ArgoprojV1alpha1().Workflows(f.config.PipelineNamespace).Get(t.Context(), run.Origin.WorkflowName, meta.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range []*wf.Workflow{native, run.Admission.Workflow} {
		got := map[string]string{}
		for _, p := range w.Spec.Arguments.Parameters {
			if admissioncontract.IsReleaseParameter(p.Name) && p.Value != nil {
				got[p.Name] = p.Value.String()
			}
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatal("release parameters differ", got, want)
		}
	}
}

func (f *composedFixture) checkReleaseTagChanges(t *testing.T, run stateport.Run, want map[string]string) {
	t.Helper()
	// Local renaming and a moved-tag push cannot rewrite a sealed admission.
	f.git.Git(t, f.git.Work, "tag", "renamed", run.ObjectSHA)
	f.git.Git(t, f.git.Work, "push", "oberth", "refs/tags/renamed")
	ordinary := f.waitMatchingRun(t, func(r stateport.Run) bool { return r.Ref == "refs/tags/renamed" && r.Origin.WorkflowUID != "" })
	f.assertReleaseParameters(t, ordinary, map[string]string{})
	f.deliverSource(t, ordinary)
	f.git.Git(t, f.git.Work, "tag", "-f", "v1.2.3", "HEAD")
	if output, err := f.pushResult(t, "refs/tags/v1.2.3", "--force"); err == nil {
		t.Fatal("moved tag accepted", output)
	}
	f.assertReleaseParameters(t, run, want)
	f.git.Write(t, "newer-default", []byte("default changed after admission"))
	f.git.CommitFile(t, "advance after release admission")
	f.git.Git(t, f.git.Work, "push", f.git.Bare, "HEAD:refs/heads/main")
	// Rerun through the real application keeps the original default head and build facts.
	if err := f.owners.c.store.Finish(t.Context(), stateport.Action{Actor: "administrator"}, stateport.RunUpdate{ID: run.ID, Revision: run.Revision, Status: stepmodel.RunFailed, Reason: "build command failed"}); err != nil {
		t.Fatal(err)
	}
	who := f.joinedActor(t)
	who.Admin = true
	request := service.StartRequest{Request: model.CIRequest{ReceiveID: "release-rerun", Repo: gittest.Repository, Ref: run.Ref, ObjectSHA: run.ObjectSHA, CommitSHA: run.CommitSHA, Trigger: model.TriggerTag, Actor: who}, RerunOf: run.ID}
	receipt, err := f.owners.wiring.StartRun(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	rerun, err := f.owners.c.store.Run(t.Context(), receipt.ID)
	if err != nil {
		t.Fatal(err)
	}
	f.assertReleaseParameters(t, rerun, want)
	request.Request.ReceiveID, request.Request.Assist = "changed-release-rerun", true
	if receipt, err := f.owners.wiring.StartRun(t.Context(), request); err == nil {
		t.Fatal("changed release rerun admitted", receipt)
	}
}

func checkComposedReleaseFacts(t *testing.T, kind string) {
	t.Helper()
	f := composeFixture(t, composeBinary(t))
	commit := f.prepareCommit(t)
	f.prepareAdministrator(t)
	cancel, done := f.run(t)
	defer cancel()
	f.checkStarting(t, done)
	f.waitStarted(t, done)
	f.bindClient(t)
	build := f.releaseBuild(t, "refs/heads/tested", kind != "failed")
	if kind == "annotated" {
		f.git.Git(t, f.git.Work, "tag", "-a", "v1.2.3", "-m", "release", commit)
	} else {
		f.git.Git(t, f.git.Work, "tag", "v1.2.3", commit)
	}
	if kind == "failed" {
		output, err := f.pushResult(t, "refs/tags/v1.2.3")
		if err == nil || !strings.Contains(output, "release build has not passed") || !strings.Contains(output, "wait for it to pass") {
			t.Fatal(output, err)
		}
		native, err := f.argo.ArgoprojV1alpha1().Workflows(f.config.PipelineNamespace).List(t.Context(), meta.ListOptions{})
		if err != nil || len(native.Items) != 1 || native.Items[0].Name != build.Origin.WorkflowName {
			t.Fatal("refused tag started a run", err)
		}
		if _, err := f.owners.cache.ResolveRef(t.Context(), gittest.Repository, "refs/tags/v1.2.3"); err == nil {
			t.Fatal("refused tag was stored")
		}
		f.releaseBuild(t, "refs/heads/retested", true)
		f.git.Git(t, f.git.Work, "push", "oberth", "refs/tags/v1.2.3")
		retry := f.waitMatchingRun(t, func(r stateport.Run) bool { return r.Ref == "refs/tags/v1.2.3" && r.Origin.WorkflowUID != "" })
		f.assertReleaseParameters(t, retry, admissioncontract.ReleaseFacts{Version: "v1.2.3", TestedCommit: commit, DefaultCommit: commit, BuildStatus: "passed"}.Parameters())
		return
	}
	// Admission records an ancestor's containing head, not the tag commit twice.
	f.git.Write(t, "after-build", []byte("next default head"))
	head := f.git.CommitFile(t, "advance default")
	f.git.Git(t, f.git.Work, "push", f.git.Bare, "HEAD:refs/heads/main")
	f.git.Git(t, f.git.Work, "push", "oberth", "refs/tags/v1.2.3")
	run := f.waitMatchingRun(t, func(r stateport.Run) bool { return r.Ref == "refs/tags/v1.2.3" && r.Origin.WorkflowUID != "" })
	want := admissioncontract.ReleaseFacts{Version: "v1.2.3", TestedCommit: commit, DefaultCommit: head, BuildStatus: "passed"}.Parameters()
	f.assertReleaseParameters(t, run, want)
	f.deliverSource(t, run)
	f.checkReleaseTagChanges(t, run, want)
}
