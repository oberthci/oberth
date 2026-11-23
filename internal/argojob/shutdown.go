package argojob

import (
	wf "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	"github.com/oberthci/oberth/internal/stepmodel"
)

// A workflow shutdown does not change failures that were already observed.
// Pod assessment can replace the native shutdown message with the exit code.
func stoppedNode(in projectionInput, n wf.NodeStatus) bool {
	if !in.w.Spec.Shutdown.Enabled() || !terminalNode(n) {
		return false
	}
	if in.w.Spec.Shutdown.ShouldExecute(exitHandler(n, in.w.Status.Nodes)) {
		return false
	}
	for _, old := range in.previous {
		if latestNode(old.Attempts) == n.ID && terminalStep(old.Status) {
			return old.FailureClass == stepmodel.FailureCancelled
		}
	}
	if nativeShutdown(n.Message) {
		return true
	}
	for _, old := range in.previousNodes {
		if old.NodeID == n.ID {
			return old.Status == stepmodel.StepRunning || old.Status == stepmodel.StepQueued || old.Status == stepmodel.StepPending
		}
	}
	return false
}

func exitHandler(n wf.NodeStatus, nodes wf.Nodes) bool {
	for range len(nodes) + 1 {
		if n.IsExitNode() {
			return true
		}
		parent, ok := nodes[n.BoundaryID]
		if !ok {
			return false
		}
		n = parent
	}
	return false
}

func nativeShutdown(message string) bool {
	return message == "workflow shutdown with strategy:  Stop" || message == "workflow shutdown with strategy:  Terminate" ||
		message == "workflow shutdown with strategy: Stop" || message == "workflow shutdown with strategy: Terminate" ||
		message == "Stopped with strategy 'Stop'" || message == "Stopped with strategy 'Terminate'"
}
