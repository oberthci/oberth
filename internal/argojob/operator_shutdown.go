package argojob

import wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"

// Native stop/terminate intent is structured Workflow state, not a substring
// inferred from a step error. Preserve Argo's message through diagnostics.
func operatorShutdown(workflow *wfv1.Workflow) bool {
	return workflow.Status.Phase.Completed() && workflow.Status.Phase != wfv1.WorkflowSucceeded &&
		(workflow.Spec.Shutdown == wfv1.ShutdownStrategyStop || workflow.Spec.Shutdown == wfv1.ShutdownStrategyTerminate)
}
