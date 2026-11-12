package argojob

import (
	"encoding/json"
	"strings"
	"testing"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	"k8s.io/utils/ptr"

	"github.com/oberthci/oberth/internal/model"
)

func TestNativeSuspensionObservationUsesLiveArgoState(t *testing.T) {
	for _, kind := range []string{"workflow", "step", "resumed", "finished-step", "terminal"} {
		t.Run(kind, func(t *testing.T) {
			wf := schedulingWorkflow()
			wf.Status.Message = "PRIVATE controller text"
			switch kind {
			case "workflow":
				wf.Spec.Suspend = ptr.To(true)
			case "step":
				wf.Status.Nodes["hold"] = wfv1.NodeStatus{Type: wfv1.NodeTypeSuspend, Phase: wfv1.NodeRunning, Message: "PRIVATE"}
			case "resumed":
				wf.Spec.Suspend = ptr.To(false)
			case "finished-step":
				wf.Status.Nodes["hold"] = wfv1.NodeStatus{Type: wfv1.NodeTypeSuspend, Phase: wfv1.NodeSucceeded}
			case "terminal":
				wf.Spec.Suspend = ptr.To(true)
				wf.Status.Phase = wfv1.WorkflowSucceeded
			}
			controller := &Controller{workflows: &schedulingWorkflowClient{workflow: wf}, config: Config{Namespace: "pipeline"}}
			observation, err := controller.ObserveScheduling(t.Context(), "wf", "run", "sha")
			if err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "workflow":
				if observation.Reason != "Suspended" || observation.Message != model.WorkflowSuspendedMessage {
					t.Fatal(observation)
				}
			case "step":
				if observation.Reason != "Suspended" || observation.Message != model.StepSuspendedMessage {
					t.Fatal(observation)
				}
			default:
				if observation.Reason == "Suspended" {
					t.Fatal("stale suspension", observation)
				}
			}
			raw, _ := json.Marshal(observation)
			if strings.Contains(string(raw), "PRIVATE") {
				t.Fatal("raw Argo text escaped", string(raw))
			}
		})
	}
}
