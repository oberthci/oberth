package service

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/pkg/argoworkflow"
)

type validationFailureJobs struct {
	*fakeJobs
	source                string
	receiptErr, createErr error
	creates               int
}

func (j *validationFailureJobs) PipelineDemandReceipt(context.Context, WorkflowRequest) (*argoworkflow.DemandReceipt, error) {
	if j.receiptErr != nil {
		return nil, j.receiptErr
	}
	wf, err := argoworkflow.Decode([]byte(j.source))
	if err != nil {
		return nil, err
	}
	return nil, argoworkflow.Admit(wf, argoworkflow.Policy{})
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
	for _, stage := range []string{"receipt-admission", "receipt-decode", "create", "receipt-io", "create-io"} {
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
			case "create":
				jobs.createErr = fmt.Errorf("controller build: %w", &argoworkflow.AdmissionError{Cause: errors.New(rule)})
			case "receipt-io":
				jobs.receiptErr = errors.New("source read failed")
			case "create-io":
				jobs.createErr = errors.New("Workflow API unavailable")
			}
			f.scheduler.jobs = jobs
			request := CIRequest{EventID: "validation", Repository: f.repo, Branch: "main", SHA: strings.Repeat("a", 40), Actor: "author@host"}
			accepted, err := f.scheduler.EnqueueCI(t.Context(), request)
			if err != nil || accepted.Status != model.RunQueued {
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
			replay, err := f.scheduler.EnqueueCI(t.Context(), request)
			if err != nil || !replay.Duplicate || replay.ID != run.ID {
				t.Fatal("validation changed accepted event binding", replay, err)
			}
		})
	}
}
