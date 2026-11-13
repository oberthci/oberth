package argojob

import wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"

// Argo v4.0.8's controller attributes a failed/error outcome to shutdown with
// either canonical message (workflow/controller/operator.go). Intent alone is
// not evidence: its shutdown utility can patch spec after a genuine failure
// ends, or change the strategy after an earlier shutdown completes.
func operatorShutdown(workflow *wfv1.Workflow) bool {
	if !workflow.Status.Phase.Completed() || workflow.Status.Phase == wfv1.WorkflowSucceeded {
		return false
	}
	switch workflow.Spec.Shutdown {
	case wfv1.ShutdownStrategyStop, wfv1.ShutdownStrategyTerminate:
		return workflow.Status.Message == "Stopped with strategy 'Stop'" ||
			workflow.Status.Message == "Stopped with strategy 'Terminate'"
	default:
		return false
	}
}
