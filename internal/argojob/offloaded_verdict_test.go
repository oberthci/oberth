package argojob

import (
	"io"
	"testing"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
)

func TestReviewOffloadedStatusPreservesNativeVerdict(t *testing.T) {
	for _, mode := range []string{"inline", "offloaded"} {
		for _, path := range []string{"wait", "terminal"} {
			t.Run(mode+"/"+path, func(t *testing.T) {
				wf := replayTerminalWorkflow()
				wf.Namespace, wf.UID = "pipeline", "workflow-uid"
				wf.Annotations = map[string]string{runIDAnnotation: "run"}
				wf.Status.Phase = wfv1.WorkflowSucceeded
				for id, node := range wf.Status.Nodes {
					node.Phase = wfv1.NodeSucceeded
					wf.Status.Nodes[id] = node
				}
				if mode == "offloaded" {
					wf.Status.Nodes = nil
					wf.Status.OffloadNodeStatusVersion = "native-offload-version"
				}
				controller := &Controller{workflows: &staticWorkflowClient{workflow: wf}, config: Config{Namespace: wf.Namespace}, hub: containerLogCache{workflow: wf}}
				var got *Completion
				var err error
				if path == "wait" {
					value, waitErr := controller.Wait(t.Context(), wf.Name, "run", io.Discard)
					got, err = &value, waitErr
				} else {
					got, err = controller.TerminalState(t.Context(), wf.Name)
				}
				if err != nil || got == nil || !got.Terminal || !got.Succeeded {
					t.Fatalf("native Succeeded verdict lost: completion=%+v error=%v", got, err)
				}
				if mode == "offloaded" && got.ObservationComplete {
					t.Fatal("unavailable nodes granted replay authority")
				}
			})
		}
	}
}
