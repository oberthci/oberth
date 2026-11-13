package app

import (
	"context"
	"testing"

	"github.com/oberthci/oberth/internal/argojob"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/service"
)

func TestReleasePreflightWithoutStoreReadsSkipsAccessGate(t *testing.T) {
	loader := &failingAdmissionGrants{}
	jobs := &ArgoJobs{secretAccess: loader, reconcilerHealthy: &fakeReconciler{healthy: false}}
	const source = `apiVersion: argoproj.io/v1alpha1
kind: Workflow
spec:
  entrypoint: main
  templates:
  - name: main
    container:
      image: oberth
      command: ["/bin/true"]
`
	result, err := jobs.PlanRelease(t.Context(), service.WorkflowRequest{Repository: model.Repository{ID: 42, Name: "repo"}}, []byte(source))
	if err != nil || loader.calls != 0 || len(result.Problems) != 0 {
		t.Fatal("plain preflight depended on secret access", result, err, loader.calls)
	}
}

func TestReleasePreflightRecordsDeniedCanonicalDeclarations(t *testing.T) {
	recorder := &fakeDeclarationRecorder{}
	jobs := &ArgoJobs{declarations: recorder}
	problems, err := jobs.PreflightRelease(context.Background(), service.WorkflowRequest{
		Repository: model.Repository{ID: 42, Name: "repo"}, UpstreamName: "forge", UpstreamOrg: "org",
		Run: model.Run{SHA: "object", TestedSHA: "commit"},
	}, []byte(declarationWorkflow))
	if err != nil {
		t.Fatal(err)
	}
	if len(problems.Problems) == 0 || !recorder.called || recorder.repo != "forge/org/repo" || recorder.sha != "commit" || len(recorder.declarations) != 2 {
		t.Fatalf("problems=%v recorder=%+v", problems, recorder)
	}
}

type changingPreflightGrants struct {
	grants map[string]map[string]bool
	calls  int
}

func (loader *changingPreflightGrants) ActiveSecretGrants(context.Context, int64, string) (map[string]map[string]bool, error) {
	loader.calls++
	return loader.grants, nil
}

func TestReleasePreflightReloadsGrantsAfterApproval(t *testing.T) {
	loader := &changingPreflightGrants{}
	jobs := &ArgoJobs{secretAccess: loader, config: argojob.Config{PerStepIdentities: map[string]argojob.PerRepoIdentityConfig{"forge/org/repo/.oberth/release.yaml/*": {ServiceAccountName: "scoped-file"}}}}
	request := service.WorkflowRequest{Repository: model.Repository{ID: 42, Name: "repo"}, UpstreamName: "forge", UpstreamOrg: "org"}
	problems, err := jobs.PreflightRelease(context.Background(), request, []byte(declarationWorkflow))
	if err != nil || len(problems.Problems) == 0 {
		t.Fatalf("missing grant: %v %v", problems, err)
	}
	loader.grants = map[string]map[string]bool{"*": {"oberth/data/release/r2-token": true, "oberth/data/release/cosign-key": true}}
	problems, err = jobs.PreflightRelease(context.Background(), request, []byte(declarationWorkflow))
	if err != nil || len(problems.Problems) != 0 || loader.calls != 2 {
		t.Fatalf("fresh grants: %v %v calls=%d", problems, err, loader.calls)
	}
}

func TestReleasePlanDoesNotRecordDeclarations(t *testing.T) {
	recorder := &fakeDeclarationRecorder{}
	jobs := &ArgoJobs{declarations: recorder}
	result, err := jobs.PlanRelease(context.Background(), service.WorkflowRequest{
		Repository: model.Repository{ID: 42, Name: "repo"}, UpstreamName: "forge", UpstreamOrg: "org",
	}, []byte(declarationWorkflow))
	if err != nil || len(result.Problems) == 0 || recorder.called {
		t.Fatalf("result=%+v err=%v recorder=%+v", result, err, recorder)
	}
}
