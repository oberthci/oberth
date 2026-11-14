package service

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/argojob"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/pkg/argoworkflow"
	"github.com/oberthci/oberth/pkg/pipeline"
)

type validationFailureJobs struct {
	*fakeJobs
	source                string
	receiptErr, createErr error
	creates               int
}

func (j *validationFailureJobs) ResolvePipelineSource(context.Context, WorkflowRequest) (*argoworkflow.SourceReceipt, error) {
	if j.receiptErr != nil {
		return nil, j.receiptErr
	}
	resolved, err := argoworkflow.ResolveRunSource([]byte(j.source), argoworkflow.RunSourceBinding{})
	if err != nil {
		return nil, err
	}
	return nil, argoworkflow.Admit(resolved.Workflow(), argoworkflow.Policy{})
}
func (j *validationFailureJobs) CreateWorkflow(ctx context.Context, request WorkflowRequest) error {
	j.creates++
	if j.createErr != nil {
		return j.createErr
	}
	return j.fakeJobs.CreateWorkflow(ctx, request)
}

func TestQueuedWorkflowValidationRefusalIsNotInfrastructure(t *testing.T) {
	const valid = `apiVersion: argoproj.io/v1alpha1
kind: Workflow
spec:
  entrypoint: main
  templates:
  - name: main
    container:
      image: golang:1.26-alpine@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
      command: [/bin/true]
`
	const rule = "argoworkflow: spec.hostNetwork is not admitted"
	for _, stage := range []string{"receipt-admission", "receipt-decode", "receipt-reference", "create", "receipt-io", "create-io"} {
		t.Run(stage, func(t *testing.T) {
			f := newControlFixture(t)
			jobs := &validationFailureJobs{fakeJobs: f.jobs, source: valid}
			expected := rule
			switch stage {
			case "receipt-admission":
				jobs.source = strings.Replace(valid, "  entrypoint:", "  hostNetwork: true\n  entrypoint:", 1)
			case "receipt-decode":
				jobs.source = "apiVersion: argoproj.io/v1alpha1\nkind: Secret\n"
				_, err := argoworkflow.Decode([]byte(jobs.source))
				expected = err.Error()
			case "receipt-reference":
				jobs.source = strings.Replace(valid, "  entrypoint:", "  workflowTemplateRef: {name: unsupported}\n  entrypoint:", 1)
				workflow, failure := argoworkflow.Decode([]byte(jobs.source))
				if failure != nil {
					t.Fatal(failure)
				}
				expected = argoworkflow.Admit(workflow, argoworkflow.Policy{}).Error()
			case "create":
				jobs.createErr = fmt.Errorf("controller build: %w", &argoworkflow.AdmissionError{Cause: errors.New(rule)})
			case "receipt-io":
				jobs.receiptErr = errors.New("source read failed")
			case "create-io":
				jobs.createErr = errors.New("Workflow API unavailable")
			}
			f.scheduler.jobs = jobs
			request := CIRequest{EventID: "validation", Repository: f.repo, Branch: "main", SHA: strings.Repeat("a", 40), Actor: "author@host"}
			accepted, err := f.scheduler.recordCIForTest(t.Context(), request)
			if err != nil || accepted.Status != model.RunRunning {
				t.Fatal(accepted, err)
			}
			if jobs.creates != 0 {
				t.Fatal("enqueue started Workflow creation")
			}
			if err = f.scheduler.ProcessNext(t.Context()); err != nil {
				t.Fatal(err)
			}
			run, err := f.store.Run(t.Context(), accepted.ID)
			if err != nil || run.Status != model.RunFailed {
				t.Fatal(run, err)
			}
			if strings.HasSuffix(stage, "-io") {
				if run.Phase != "infrastructure" || run.FailureCause != "" {
					t.Fatal("real I/O failure reclassified", run)
				}
				return
			}
			if run.Phase != "job" || run.Error != expected || !strings.HasPrefix(presentationRun(run).DisplayReason, validationRefusalPrefix) {
				t.Fatal("validation refusal mislabeled or lost rule", run, presentationRun(run))
			}
			hint := runRetryHint(run, nil)
			if hint == nil || hint.Class != model.RetryCode || hint.Cause != "validation" {
				t.Fatal("unchanged retry advised", hint)
			}
			wantCreates := 0
			if stage == "create" {
				wantCreates = 1
			}
			if jobs.creates != wantCreates {
				t.Fatal("unexpected Create calls", jobs.creates)
			}
			replay, err := f.scheduler.recordCIForTest(t.Context(), request)
			if err != nil || !replay.Duplicate || replay.ID != run.ID {
				t.Fatal("validation changed accepted event binding", replay, err)
			}
		})
	}
}

func TestRealBuildErrorsKeepOperatorConfigurationOutOfValidationHints(t *testing.T) {
	const source = `apiVersion: argoproj.io/v1alpha1
kind: Workflow
spec:
  entrypoint: main
  templates:
  - name: main
    container:
      image: golang:1.26-alpine@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
      command: [/bin/true]
`
	for _, kind := range []string{"document", "configuration", "vault-address", "ci-role", "identity"} {
		t.Run(kind, func(t *testing.T) {
			f := newControlFixture(t)
			config := argojob.Config{Namespace: "pipeline", PipelineServiceAccount: "pipeline", CredentialedServiceAccount: "release", CISecretsServiceAccount: "ci", ExecutorServiceAccount: "executor", VaultAddress: "https://vault.example.test:8200", VaultCISecretsRole: "ci", VaultCredentialedRole: "release"}
			request := argojob.Request{RunID: "run", RepositoryID: f.repo.ID, Name: "workflow", Repo: "oberth", UpstreamOrg: "acme", SHA: strings.Repeat("a", 40), Ref: "main", Trigger: pipeline.TriggerCI, Source: []byte(source), ApprovedSecrets: map[string]bool{}}
			switch kind {
			case "document":
				request.Source = []byte(strings.Replace(source, "spec:", "spec:\n  hostNetwork: true", 1))
			case "configuration":
				config.Namespace = "NOT VALID"
			case "identity":
				config.ExecutorServiceAccount = ""
			default:
				request.Source = []byte(strings.Replace(source, "spec:", "metadata:\n  annotations:\n    oberth.ci/secret-paths: oberth/upstream/acme/oberth/test-secret\nspec:", 1))
				request.ApprovedSecrets["oberth/upstream/acme/oberth/test-secret"] = true
				request.StepGrants = map[string]map[string]bool{"oberth/upstream/acme/oberth/test-secret": {"*": true}}
				request.Source = []byte(strings.Replace(string(request.Source), "command: [/bin/true]", "command: [oberth, secretstore, exec, --path=oberth/upstream/acme/oberth/test-secret, --, /bin/true]", 1))
				if kind == "vault-address" {
					config.VaultAddress = ""
					config.VaultCredentialedRole = ""
				} else {
					config.VaultCISecretsRole = ""
				}
			}
			_, failure := argojob.Build(config, request)
			if failure == nil {
				t.Fatal("Build did not fail", kind)
			}
			accepted, err := f.scheduler.recordCIForTest(t.Context(), CIRequest{EventID: kind, Repository: f.repo, Branch: "main", SHA: request.SHA, Actor: "owner"})
			if err != nil {
				t.Fatal(err)
			}
			run, err := f.store.StartRun(t.Context(), accepted.ID)
			if err != nil || run.ID != accepted.ID {
				t.Fatal(run, err)
			}
			if err = f.scheduler.finishInfrastructureFailure(t.Context(), run, f.repo, fmt.Errorf("create Job: %w", failure)); err != nil {
				t.Fatal(err)
			}
			ended, err := f.store.Run(t.Context(), run.ID)
			if err != nil {
				t.Fatal(err)
			}
			reason := presentationRun(ended).DisplayReason
			hint := runRetryHint(ended, nil)
			if kind == "document" {
				if ended.Phase != "job" || !strings.HasPrefix(reason, validationRefusalPrefix) || hint == nil || hint.Class != model.RetryCode {
					t.Fatal("document rule lost refusal classification", ended, hint)
				}
			} else if ended.Phase != "infrastructure" || strings.Contains(reason, "validation refused") || (hint != nil && hint.Class == model.RetryCode) {
				t.Fatal("operator configuration blamed on YAML", ended, hint)
			}
		})
	}
}
