package app

import (
	"testing"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/storetest"
)

func TestExecutionTargetBranchDoesNotCaptureSameNamedTag(t *testing.T) {
	f := newTargetRouteFixture(t)
	ctx := t.Context()
	writePipeline(t, f.request.SourceDir, "release.yaml", groupedPipeline(""))
	request := f.request
	result, err := storetest.EnqueueRun(f.db, ctx, model.RunSpec{RepoID: request.Repository.ID, RefKind: model.RefTag, Ref: request.Run.Ref, SHA: request.Run.SHA, Actor: "fixture", Trigger: "tag", Release: true})
	if err != nil {
		t.Fatal(err)
	}
	request.Run, err = f.db.SetRunWorkflowName(ctx, result.ID, "local-release-tag")
	if err != nil {
		t.Fatal(err)
	}
	request.WorkflowName = request.Run.WorkflowName
	if size, err := f.local.PipelineDemandReceipt(ctx, request); err != nil {
		t.Fatal("local release control", size, err)
	}
	if size, err := f.jobs.PipelineDemandReceipt(ctx, request); err != nil {
		t.Fatal("branch target captured tag release", size, err)
	}
	binding, found, err := f.db.ExecutionTarget(ctx, request.Run.ID, request.WorkflowName)
	if err != nil || !found || binding.Target != "local" || binding.RefKind != string(model.RefTag) {
		t.Fatal("local tag binding lost", binding, found, err)
	}
	restarted, err := WithExecutionTargets(f.local, f.db, []ExecutionTarget{f.definition})
	if err != nil {
		t.Fatal(err)
	}
	if size, err := restarted.PipelineDemandReceipt(ctx, request); err != nil {
		t.Fatal("tag restart changed route", size, err)
	}
	// Canonical remote custody is checked before initial branch selection. A
	// changed request cannot escape that binding by presenting itself as a tag.
	if _, err := f.jobs.PipelineDemandReceipt(ctx, f.request); err != nil {
		t.Fatal(err)
	}
	changed := f.request
	changed.Run.RefKind, changed.Run.Trigger, changed.Run.Release = model.RefTag, "tag", true
	if _, err := f.jobs.PipelineDemandReceipt(ctx, changed); err == nil {
		t.Fatal("changed remote binding fell back locally")
	}
	remote, found, err := f.db.ExecutionTarget(ctx, changed.Run.ID, changed.WorkflowName)
	if err != nil || !found || remote.Target != "remote" || remote.RefKind != string(model.RefBranch) {
		t.Fatal("remote binding changed", remote, found, err)
	}
	if f.remote.posts != 0 || f.localWorkflows.posts != 0 || *f.execs != 0 || len(f.kube.Actions()) != 0 || len(f.localKube.Actions()) != 0 {
		t.Fatal("planning allocated resources")
	}
}
