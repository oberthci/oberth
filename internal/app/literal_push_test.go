package app_test

import (
	"context"
	"testing"

	wf "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"

	"github.com/oberthci/oberth/internal/admissioncontract"
	"github.com/oberthci/oberth/internal/app"
	"github.com/oberthci/oberth/internal/argojob"
	nativefake "github.com/oberthci/oberth/internal/argojob/fake"
	"github.com/oberthci/oberth/internal/gitcache/gittest"
	"github.com/oberthci/oberth/internal/identityname"
	"github.com/oberthci/oberth/internal/nativebinding"
	"github.com/oberthci/oberth/internal/stateport"
	"github.com/oberthci/oberth/pkg/argoworkflow"
)

func TestOrdinaryPushRecordsLiteralTemplateRef(t *testing.T) {
	t.Parallel()
	recordOrdinaryPush(t, workflow(), "refs/heads/{{workflow.name}}")
}

func recordOrdinaryPush(t *testing.T, authored *wf.Workflow, ref string) *wf.Workflow {
	t.Helper()
	f := newFixture(t)
	git := gittest.New(t)
	git.Write(t, ".oberth/build.yaml", document(authored))
	commit := git.CommitFile(t, "literal branch workflow")
	git.Git(t, git.Work, "check-ref-format", ref)
	git.Git(t, git.Work, "push", git.Bare, "HEAD:"+ref)
	cache := git.Cache(t, nil)
	selected, err := cache.ResolveRef(t.Context(), gittest.Repository, ref)
	landedOK(t, err)
	pipeline, err := identityname.Name(identityname.Key{Repository: gittest.Repository}, identityname.PipelineCI)
	landedOK(t, err)
	f.authority.LoadFunc = func(context.Context, app.Scope) (admissioncontract.PrepareRequest, argoworkflow.Policy, error) {
		return admissioncontract.PrepareRequest{IdentityRequest: admissioncontract.IdentityRequest{Namespace: "runs", PipelineAccount: pipeline}}, argoworkflow.Policy{}, nil
	}
	native := &nativefake.Workflows{}
	executor := argojob.New(argojob.Config{Accounts: nativefake.Accounts{Names: map[string]string{pipeline: "pipeline-uid"}}, Runs: f.runs.Store, Progress: f.runs.Store, Workflows: native, Now: landedClock})
	deps := f.d
	deps.Source, deps.Authority, deps.Native = cache, f.authority, executor
	submitter, err := app.New(deps)
	landedOK(t, err)
	t.Cleanup(func() { landedOK(t, submitter.Close(context.Background())) })
	pushed := receive()
	pushed.Source = cache
	pushed.Updates[0].Request.Ref = selected
	_, err = submitter.Gate(t.Context(), pushed)
	landedOK(t, err)
	for range 2 {
		_, err = submitter.Deliver(t.Context(), pushed)
		landedOK(t, err)
	}
	run, err := f.runs.Run(t.Context(), "r1")
	landedOK(t, err)
	runs, err := f.runs.Runs(t.Context(), stateport.RunFilter{Limit: 10})
	landedOK(t, err)
	if len(runs) != 1 || native.Posts != 1 || run.Ref != ref || run.Admission.Ref != ref || run.CommitSHA == nil || *run.CommitSHA != commit {
		t.Fatal("ordinary push lost literal ref or repeated a run", runs, native.Posts)
	}
	assertLiteralPushNative(t, native, ref)
	if _, err := executor.Adopt(t.Context(), bindingForLiteralPush(t, f)); err != nil {
		t.Fatal("ordinary binding refused literal ref", err)
	}
	return native.Object
}

func assertLiteralPushNative(t *testing.T, native *nativefake.Workflows, ref string) {
	t.Helper()
	if native.Object.Annotations[nativebinding.RefAnnotation] != ref {
		t.Fatal("native submission changed literal ref")
	}
	for _, parameter := range native.Object.Spec.Arguments.Parameters {
		if parameter.Name == "oberth-ref" || parameter.Name == "oberth-sha" {
			t.Fatal("ordinary push acquired scheduled parameters")
		}
	}
}

func bindingForLiteralPush(t *testing.T, f *fixture) nativebinding.RunBinding {
	t.Helper()
	run, err := f.runs.Run(t.Context(), "r1")
	landedOK(t, err)
	return nativebinding.RunBinding{Workflow: nativebinding.Object{Namespace: run.Admission.Identity.Namespace, Name: run.Origin.WorkflowName, UID: run.Origin.WorkflowUID}, RepositoryID: run.RepositoryID, RunID: run.ID, Ref: run.Ref, Trigger: string(run.Origin.Trigger), Revision: run.Admission.SourceReceipt.Revision, Identity: run.Admission.Digest, SpecSHA256: run.Admission.SpecDigest}
}
