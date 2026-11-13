package app

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/argojob"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/service"
	"github.com/oberthci/oberth/pkg/argoworkflow"
)

func TestReleasePreflightWithoutStoreReadsSkipsAccessGate(t *testing.T) {
	loader := &failingAdmissionGrants{}
	jobs := &ArgoJobs{secretAccess: loader}
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

func TestReleasePreflightLoadsCurrentWorkflowCapability(t *testing.T) {
	for _, mode := range []string{"granted", "absent", "foreign-namespace", "store-error"} {
		t.Run(mode, func(t *testing.T) {
			jobs := &ArgoJobs{config: argojob.Config{Namespace: "pipelines", PipelineServiceAccount: "pipeline", CredentialedServiceAccount: "release", CISecretsServiceAccount: "ci", ExecutorServiceAccount: "executor"}}
			calls := 0
			unavailable := errors.New("capability store unavailable")
			jobs.SetWorkflowCapabilityLoader(func(_ context.Context, repoID int64) (*argoworkflow.Capability, error) {
				calls++
				if repoID != 7 {
					t.Fatalf("loaded capability for repository %d", repoID)
				}
				if mode == "store-error" {
					return nil, unavailable
				}
				if mode == "absent" {
					return nil, nil
				}
				namespace := "pipelines"
				if mode == "foreign-namespace" {
					namespace = "other"
				}
				return &argoworkflow.Capability{Namespace: namespace, ServiceAccount: "operator-runner", ResourceKinds: []argoworkflow.ResourceKindCapability{{APIVersion: "tasks.example.test/v1", Kind: "Task", Actions: []string{"create"}}}}, nil
			})
			request := service.WorkflowRequest{Repository: model.Repository{ID: 7, Name: "repo"}, PipelineFile: ".oberth/release.yaml"}
			for _, plan := range []bool{false, true} {
				var result service.ReleasePreflightResult
				var err error
				if plan {
					result, err = jobs.PlanRelease(t.Context(), request, []byte(appCapabilityWorkflow))
				} else {
					result, err = jobs.PreflightRelease(t.Context(), request, []byte(appCapabilityWorkflow))
				}
				switch mode {
				case "granted":
					if err != nil || len(result.Problems) != 0 {
						t.Fatalf("granted resource preflight: %+v %v", result, err)
					}
				case "absent":
					if !errors.Is(err, argoworkflow.ErrWorkflowCapabilityRequired) {
						t.Fatalf("missing capability: %v", err)
					}
				case "foreign-namespace":
					if err == nil || !strings.Contains(err.Error(), "different execution namespace") {
						t.Fatalf("foreign capability: %v", err)
					}
				case "store-error":
					if !errors.Is(err, unavailable) {
						t.Fatalf("capability lookup error lost: %v", err)
					}
				}
			}
			if calls != 2 {
				t.Fatalf("capability must reload for each preflight: %d", calls)
			}
		})
	}
}
