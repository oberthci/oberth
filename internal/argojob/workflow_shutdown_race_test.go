package argojob

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	argofake "github.com/argoproj/argo-workflows/v4/pkg/client/clientset/versioned/fake"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/validation/field"
	ktesting "k8s.io/client-go/testing"
)

func TestStopCannotOverwriteConcurrentTerminate(t *testing.T) {
	for _, concurrent := range []bool{true, false} {
		name := "terminate-before-stop-read-control"
		if concurrent {
			name = "terminate-after-stop-read"
		}
		t.Run(name, func(t *testing.T) {
			workflow := &wfv1.Workflow{ObjectMeta: metav1.ObjectMeta{Name: "wf", Namespace: "pipeline", UID: "original", ResourceVersion: "1", Annotations: map[string]string{runIDAnnotation: "run"}}, Status: wfv1.WorkflowStatus{Phase: wfv1.WorkflowRunning}}
			if !concurrent {
				workflow.Spec.Shutdown = wfv1.ShutdownStrategyTerminate
			}
			client := argofake.NewSimpleClientset(workflow)
			if concurrent {
				client.PrependReactor("patch", "workflows", func(ktesting.Action) (bool, runtime.Object, error) {
					// A separate cancellation has completed its Terminate patch after
					// this Cancel's GET but before this older Stop patch reaches the API.
					current := workflow.DeepCopy()
					current.Spec.Shutdown = wfv1.ShutdownStrategyTerminate
					current.ResourceVersion = "2"
					if err := client.Tracker().Update(wfv1.SchemeGroupVersion.WithResource("workflows"), current, workflow.Namespace); err != nil {
						t.Fatal(err)
					}
					return false, nil, nil // Apply the actual JSON Patch in the client tracker.
				})
			}
			controller := &Controller{workflows: client.ArgoprojV1alpha1().Workflows(workflow.Namespace)}
			if err := controller.Cancel(t.Context(), workflow.Name, "run"); err != nil {
				t.Fatal(err)
			}
			final, err := controller.workflows.Get(t.Context(), workflow.Name, metav1.GetOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if final.Spec.Shutdown != wfv1.ShutdownStrategyTerminate {
				t.Fatalf("accepted Terminate was downgraded by stale Stop: %s", final.Spec.Shutdown)
			}
		})
	}
}

func TestStopRetriesLiveComparisonWithoutStatusFence(t *testing.T) {
	for _, mode := range []string{"status", "spec", "api-invalid", "replacement", "continuous-conflict"} {
		t.Run(mode, func(t *testing.T) {
			workflow := &wfv1.Workflow{ObjectMeta: metav1.ObjectMeta{Name: "wf", Namespace: "pipeline", UID: "original", ResourceVersion: "1", Annotations: map[string]string{runIDAnnotation: "run"}}, Status: wfv1.WorkflowStatus{Phase: wfv1.WorkflowRunning}}
			client := argofake.NewSimpleClientset(workflow)
			patches := 0
			client.PrependReactor("patch", "workflows", func(ktesting.Action) (bool, runtime.Object, error) {
				patches++
				if mode == "continuous-conflict" {
					return true, nil, apierrors.NewConflict(workflowTestResource, workflow.Name, fmt.Errorf("admission comparison refused attempt %d", patches))
				}
				if patches > 1 {
					return false, nil, nil
				}
				current, err := client.Tracker().Get(wfv1.SchemeGroupVersion.WithResource("workflows"), workflow.Namespace, workflow.Name)
				if err != nil {
					t.Fatal(err)
				}
				changed := current.(*wfv1.Workflow).DeepCopy()
				changed.ResourceVersion = "2"
				switch mode {
				case "status":
					changed.Status.Message = "Pod status advanced"
				case "spec":
					suspend := patches%2 == 1
					changed.Spec.Suspend = &suspend
				case "api-invalid":
					changed.Spec.Shutdown = wfv1.ShutdownStrategyTerminate
				case "replacement":
					changed.UID = "replacement"
				}
				if err := client.Tracker().Update(wfv1.SchemeGroupVersion.WithResource("workflows"), changed, workflow.Namespace); err != nil {
					t.Fatal(err)
				}
				if mode == "api-invalid" {
					return true, nil, apierrors.NewGenericServerResponse(http.StatusUnprocessableEntity, "", workflowTestResource, workflow.Name, "JSON Patch test failed", 0, false)
				}
				return false, nil, nil
			})
			controller := &Controller{workflows: client.ArgoprojV1alpha1().Workflows(workflow.Namespace)}
			err := controller.Cancel(t.Context(), workflow.Name, "run")
			if mode == "replacement" || mode == "continuous-conflict" {
				if err == nil {
					t.Fatal("changed Workflow accepted")
				}
				if mode == "continuous-conflict" && (patches != 3 || !apierrors.IsConflict(err) || !strings.Contains(err.Error(), "refused attempt 3")) {
					t.Fatalf("retry bound or last cause lost: patches=%d err=%v", patches, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			final, err := controller.workflows.Get(t.Context(), workflow.Name, metav1.GetOptions{})
			if err != nil {
				t.Fatal(err)
			}
			want := wfv1.ShutdownStrategyStop
			if mode == "api-invalid" {
				want = wfv1.ShutdownStrategyTerminate
			}
			if final.Spec.Shutdown != want {
				t.Fatalf("shutdown=%s want %s", final.Spec.Shutdown, want)
			}
			if mode == "status" && patches != 1 {
				t.Fatalf("status churn rejected shutdown: patches=%d", patches)
			}
			if mode == "spec" && (patches != 1 || final.Spec.Suspend == nil || !*final.Spec.Suspend) {
				t.Fatal("spec change was lost or unrelated fields blocked shutdown")
			}
		})
	}
}

func TestStopRetryExhaustionPreservesAdmissionCause(t *testing.T) {
	workflow := &wfv1.Workflow{ObjectMeta: metav1.ObjectMeta{Name: "workflow", Namespace: "pipeline", UID: "original", Annotations: map[string]string{runIDAnnotation: "run"}}, Status: wfv1.WorkflowStatus{Phase: wfv1.WorkflowRunning}}
	client := argofake.NewSimpleClientset(workflow)
	attempts := 0
	client.PrependReactor("patch", "workflows", func(ktesting.Action) (bool, runtime.Object, error) {
		attempts++
		return true, nil, apierrors.NewInvalid(wfv1.SchemeGroupVersion.WithKind("Workflow").GroupKind(), workflow.Name, field.ErrorList{field.Forbidden(field.NewPath("spec", "shutdown"), fmt.Sprintf("admission denied attempt %d", attempts))})
	})
	controller := &Controller{workflows: client.ArgoprojV1alpha1().Workflows(workflow.Namespace)}
	err := controller.Cancel(t.Context(), workflow.Name, "run")
	if attempts != 3 || !apierrors.IsConflict(err) || !strings.Contains(err.Error(), "admission denied attempt 3") {
		t.Fatalf("shutdown retry lost bound or admission cause: attempts=%d err=%v", attempts, err)
	}
}
