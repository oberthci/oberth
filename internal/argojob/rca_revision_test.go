package argojob

import (
	"context"
	"errors"
	"fmt"
	"testing"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"

	"github.com/oberthci/oberth/pkg/periapsis"
)

func TestRCARevisionPreparationProofRequiresAbsenceAndNoPost(t *testing.T) {
	for _, kind := range []string{"absent-seed-outage", "unknown-get", "foreign-name"} {
		t.Run(kind, func(t *testing.T) {
			config := seedTestConfig()
			kube := fake.NewClientset()
			client := &createRetryWorkflowClient{}
			kube.PrependReactor("create", "persistentvolumeclaims", func(ktesting.Action) (bool, runtime.Object, error) {
				return true, nil, apierrors.NewServiceUnavailable("storage unavailable")
			})
			if kind == "unknown-get" {
				client.getHook = func(context.Context, string) (*wfv1.Workflow, error) {
					return nil, apierrors.NewServiceUnavailable("cannot establish absence")
				}
			}
			if kind == "foreign-name" {
				client.stored = &wfv1.Workflow{}
			}
			controller, err := NewController(client, kube, config)
			if err != nil {
				t.Fatal(err)
			}
			var seeds []seedExecCall
			controller.WithSourceSeeder(NewSourceSeeder(kube, recordingExec(&seeds), config))
			request := testRequest(periapsis.TriggerCI, greedyDocument)
			request.SourceDir = writeCheckout(t)
			_, err = controller.Create(t.Context(), request)
			if err == nil || len(client.posts) != 0 {
				t.Fatal("fixture posted or did not fail", err, len(client.posts))
			}
			var proof interface {
				BeforeWorkflowCreate() bool
				InfrastructureFailure() bool
			}
			eligible := errors.As(err, &proof) && proof.BeforeWorkflowCreate() && proof.InfrastructureFailure()
			if eligible != (kind == "absent-seed-outage") {
				t.Fatal("unproven or hard failure became retryable", kind, eligible, err)
			}
		})
	}
	for _, cause := range []error{errors.New("bad pipeline"), errors.Join(context.DeadlineExceeded, errors.New("policy denied")), fmt.Errorf("seed: %w", errors.Join(context.DeadlineExceeded, errors.New("policy denied")))} {
		if (&beforeWorkflowCreateFailure{cause: cause}).InfrastructureFailure() {
			t.Fatal("non-infra cause authorized retry", cause)
		}
	}
}
