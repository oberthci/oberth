package app

import (
	"context"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/argojob"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/service"
	"github.com/oberthci/oberth/pkg/argoworkflow"
)

const appCapabilityWorkflow = `apiVersion: argoproj.io/v1alpha1
kind: Workflow
metadata:
  annotations: {example.com/tier: M}
spec:
  entrypoint: main
  activeDeadlineSeconds: 300
  templates:
  - name: main
    resource:
      action: create
      manifest: |
        apiVersion: tasks.example.test/v1
        kind: Task
        metadata: {name: '{{workflow.name}}-task', namespace: pipelines}
`

func TestWorkflowCapabilityLoadedForSizingAndSubmission(t *testing.T) {
	for _, revoke := range []bool{false, true} {
		t.Run(map[bool]string{false: "granted", true: "revoked after sizing"}[revoke], func(t *testing.T) {
			dir := t.TempDir()
			writePipeline(t, dir, "build.yaml", appCapabilityWorkflow)
			controller := &routeController{}
			jobs := &ArgoJobs{controller: controller, auditor: &stubAuditor{}, secretAccess: emptySecretAccess{}, intents: map[string]argoIntent{}, config: argojob.Config{Namespace: "pipelines", PipelineServiceAccount: "pipeline", CredentialedServiceAccount: "release", CISecretsServiceAccount: "ci", ExecutorServiceAccount: "executor"}}
			setter, ok := any(jobs).(interface {
				SetWorkflowCapabilityLoader(func(context.Context, int64) (*argoworkflow.Capability, error))
			})
			if !ok {
				t.Fatal("application does not load workflow capability grants")
			}
			calls := 0
			setter.SetWorkflowCapabilityLoader(func(_ context.Context, repoID int64) (*argoworkflow.Capability, error) {
				calls++
				if repoID != 7 {
					t.Fatalf("grant loaded for repository %d", repoID)
				}
				if revoke && calls > 1 {
					return nil, nil
				}
				return &argoworkflow.Capability{Namespace: "pipelines", ServiceAccount: "operator-runner", ResourceKinds: []argoworkflow.ResourceKindCapability{{APIVersion: "tasks.example.test/v1", Kind: "Task", Actions: []string{"create"}}}}, nil
			})
			request := service.WorkflowRequest{WorkflowName: "workflow", Run: model.Run{ID: "run", RepoID: 7, Trigger: "branch", RefKind: model.RefBranch, Ref: "main", SHA: strings.Repeat("b", 40), Actor: "test@example.test"}, Repository: model.Repository{ID: 7, Name: "repo"}, SourceDir: dir}
			resources, err := jobs.ResolvePipelineSource(t.Context(), request)
			if err != nil || resources == nil {
				t.Fatalf("sizing: %+v %v", resources, err)
			}
			request.SourceReceipt = resources
			err = jobs.CreateWorkflow(t.Context(), request)
			if revoke {
				if err == nil || controller.request.Name != "" {
					t.Fatal("revoked grant reached submission")
				}
			} else if err != nil || controller.request.Capability == nil || controller.request.Capability.ServiceAccount != "operator-runner" || calls != 2 {
				t.Fatalf("submission lost the current grant: calls=%d err=%v", calls, err)
			}
		})
	}
}

func TestWorkflowCapabilityAdoptionUsesDurableRepository(t *testing.T) {
	controller := &adoptingArgoControl{found: true}
	jobs := &ArgoJobs{controller: controller, intents: map[string]argoIntent{}}
	if _, err := jobs.Adopt(t.Context(), model.Run{ID: "run", RepoID: 7, WorkflowName: "workflow", Trigger: "branch"}); err != nil {
		t.Fatal(err)
	}
	if controller.request.RepositoryID != 7 {
		t.Fatal("recovery lost the durable repository binding")
	}
}
