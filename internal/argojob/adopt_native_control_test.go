package argojob

import (
	"io"
	"strings"
	"testing"
	"time"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	"k8s.io/utils/ptr"
)

func TestAdoptedNativeControlsPreserveRecoveryIdentity(t *testing.T) {
	for _, operation := range []string{"resume", "suspend", "stop", "terminate"} {
		t.Run(operation, func(t *testing.T) {
			wf, request := adoptionWorkflow()
			wf.Spec.Suspend = ptr.To(true)
			wf.Status.Phase = wfv1.WorkflowRunning
			// A new controller represents the process after restart.
			c := &Controller{config: Config{Namespace: wf.Namespace, MaxRunLogBytes: 4096}, workflows: &staticWorkflowClient{workflow: wf}, idleInterval: time.Millisecond}
			binding, found, err := c.Adopt(t.Context(), request)
			if err != nil || !found {
				t.Fatal("suspended Workflow not adopted", found, err)
			}
			switch operation {
			case "resume":
				wf.Spec.Suspend = nil
			case "suspend":
				wf.Spec.Suspend = ptr.To(false)
				if !binding.matches(wf) {
					t.Fatal("clearing suspension changed identity")
				}
				wf.Spec.Suspend = ptr.To(true)
			case "stop":
				wf.Spec.Shutdown = wfv1.ShutdownStrategyStop
			case "terminate":
				wf.Spec.Shutdown = wfv1.ShutdownStrategyTerminate
			}
			if !binding.matches(wf) {
				t.Fatal("permitted native control changed identity")
			}
			wf.Spec.Suspend = nil
			wf.Status.Phase = wfv1.WorkflowSucceeded
			completion, err := withWaitTestHub(t, c).WaitResumed(t.Context(), wf.Name, request.RunID, io.Discard, binding)
			if err != nil || !completion.Succeeded {
				t.Fatal("native control after restart lost completion", completion, err)
			}
		})
	}
}

func TestNativeControlsDoNotUnbindAdoptedExecution(t *testing.T) {
	for _, field := range []string{"source", "account", "parallelism", "shutdown", "capability"} {
		t.Run(field, func(t *testing.T) {
			wf, request := adoptionWorkflow()
			c := &Controller{config: Config{Namespace: wf.Namespace}, workflows: &staticWorkflowClient{workflow: wf}}
			binding, found, err := c.Adopt(t.Context(), request)
			if err != nil || !found {
				t.Fatal(found, err)
			}
			wf.Spec.Suspend = nil
			switch field {
			case "source":
				wf.Annotations["oberth.ci/sources"] = "different sources"
			case "account":
				wf.Spec.ServiceAccountName = "other"
			case "parallelism":
				wf.Spec.Parallelism = ptr.To(int64(7))
			case "shutdown":
				wf.Spec.Shutdown = "unrecognised"
			case "capability":
				wf.Annotations[capabilityAnnotation] = strings.Repeat("a", 64)
			}
			if binding.matches(wf) {
				t.Fatal("native control allowance unbound", field)
			}
		})
	}
}
