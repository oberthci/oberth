package service

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	wffake "github.com/argoproj/argo-workflows/v4/pkg/client/clientset/versioned/fake"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/utils/ptr"

	"github.com/oberthci/oberth/internal/argojob"
	"github.com/oberthci/oberth/internal/model"
)

type startupDeadlineJobs struct {
	recoveryJobs
	controller *argojob.Controller
	client     *wffake.Clientset
	runID      string
	onDelete   func()
}

func (jobs *startupDeadlineJobs) CreateWorkflow(ctx context.Context, request WorkflowRequest) error {
	jobs.runID = request.Run.ID
	_, err := jobs.client.ArgoprojV1alpha1().Workflows("pipeline").Create(ctx, &wfv1.Workflow{
		ObjectMeta: metav1.ObjectMeta{Name: request.WorkflowName, Namespace: "pipeline", UID: "workflow-uid", CreationTimestamp: metav1.NewTime(time.Now().Add(-15 * time.Minute)), Annotations: map[string]string{"oberth.ci/run-id": request.Run.ID}},
		Spec:       wfv1.WorkflowSpec{ActiveDeadlineSeconds: ptr.To(int64(7200))}, Status: wfv1.WorkflowStatus{Phase: wfv1.WorkflowRunning},
	}, metav1.CreateOptions{})
	return err
}
func (jobs *startupDeadlineJobs) Wait(ctx context.Context, name string, writer io.Writer) (JobResult, error) {
	_, err := jobs.controller.Wait(ctx, name, jobs.runID, writer)
	return JobResult{}, err
}
func (jobs *startupDeadlineJobs) Delete(ctx context.Context, name, runID string) error {
	jobs.onDelete()
	if err := jobs.recoveryJobs.Delete(ctx, name, runID); err != nil {
		return err
	}
	return jobs.controller.Cancel(ctx, name, runID)
}

func TestStartupDeadlineFailsAndReleasesOnlyAfterCleanup(t *testing.T) {
	for _, deleteFails := range []bool{false, true} {
		t.Run(map[bool]string{false: "cleanup succeeds", true: "cleanup refused"}[deleteFails], func(t *testing.T) {
			fixture := newRecoveryFixture(t)
			run := fixture.enqueueAndClaim(t, "feature/startup-deadline", strings.Repeat("a", 40))
			client := wffake.NewSimpleClientset()
			controller, err := argojob.NewController(client.ArgoprojV1alpha1().Workflows("pipeline"), fake.NewClientset(), argojob.Config{
				Namespace: "pipeline", PipelineServiceAccount: "pipeline", CredentialedServiceAccount: "credentialed", CISecretsServiceAccount: "ci-secrets", ExecutorServiceAccount: "executor",
			})
			if err != nil {
				t.Fatal(err)
			}
			jobs := &startupDeadlineJobs{client: client, controller: controller, recoveryJobs: recoveryJobs{terminalErr: ErrJobNotTerminal}}
			if deleteFails {
				jobs.deleteErr = context.DeadlineExceeded
			}
			scheduler := fixture.scheduler(t, jobs)
			jobs.onDelete = func() {
				if got := scheduler.AdmissionSnapshot(run.QueueSequence); got == nil || got.State != "admitted" {
					t.Fatalf("reservation released before cleanup: %+v", got)
				}
			}
			err = scheduler.executeAndCleanup(t.Context(), run)
			stored, readErr := fixture.store.Run(t.Context(), run.ID)
			if readErr != nil {
				t.Fatal(readErr)
			}
			view := scheduler.AdmissionSnapshot(run.QueueSequence)
			if deleteFails {
				if err == nil || stored.Status != model.RunRunning || view == nil || view.State != "admitted" {
					t.Fatalf("unsafe release without cleanup: %+v %+v %v", stored, view, err)
				}
			} else if err != nil || stored.Status != model.RunFailed || stored.Phase != "infrastructure" || !strings.Contains(stored.Error, "no scheduled pod or progress") || view != nil && view.State == "admitted" {
				t.Fatalf("startup failure did not release and explain: %+v %+v %v", stored, view, err)
			}
			if len(jobs.deleted) != 1 {
				t.Fatalf("cleanup calls=%v", jobs.deleted)
			}
		})
	}
}
