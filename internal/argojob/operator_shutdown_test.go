package argojob

import (
	"testing"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
)

func TestOperatorShutdownKeepsArgoReasonThroughDiagnostics(t *testing.T) {
	for _, strategy := range []wfv1.ShutdownStrategy{wfv1.ShutdownStrategyStop, wfv1.ShutdownStrategyTerminate} {
		t.Run(string(strategy), func(t *testing.T) {
			wf := nestedWorkflow()
			wf.Spec.Shutdown = strategy
			wf.Status.Phase = wfv1.WorkflowFailed
			wf.Status.Message = "Stopped with strategy '" + string(strategy) + "': maintenance"
			node := wf.Status.Nodes["prepare"]
			node.Phase, node.Message = wfv1.NodeFailed, "Evicted: The node was low on resource: memory."
			wf.Status.Nodes["prepare"] = node
			got := completionFor(wf)
			for i := range got.Steps {
				got.Steps[i].restartCutReason = "lost exit during restart"
			}
			got.classifyInfrastructure()
			got.classifyRestartCut()
			if !got.OperatorStopped || got.Infrastructure || got.Interrupted || got.Reason != wf.Status.Message {
				t.Fatalf("operator intent replaced by diagnostics: %+v", got)
			}
			wf.Status.Phase = wfv1.WorkflowRunning
			if completionFor(wf).OperatorStopped {
				t.Fatal("shutdown request manufactured terminal outcome")
			}
		})
	}
}
