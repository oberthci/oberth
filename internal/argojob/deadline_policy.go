package argojob

import (
	"time"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
)

// DefaultExecutionTimeout bounds workflows that omit a repository deadline.
const DefaultExecutionTimeout = 30 * time.Minute

// ExecutionTimeout applies the administrator's global and canonical repository
// caps. A repository document may shorten these limits, never raise them.
func (config Config) ExecutionTimeout(repository string, declared *int64) time.Duration {
	config.applyDefaults()
	limit := config.WorkflowTimeout
	if cap := config.PerRepoWorkflowTimeout[repository]; cap > 0 && cap < limit {
		limit = cap
	}
	duration := DefaultExecutionTimeout
	if declared != nil && *declared > 0 {
		// Compare seconds before multiplying so untrusted int64 values cannot overflow.
		if *declared >= int64(limit/time.Second) {
			return limit
		}
		duration = time.Duration(*declared) * time.Second
	}
	if duration > limit {
		duration = limit
	}
	return duration
}

func applyExecutionDeadline(workflow *wfv1.Workflow, config Config, repository string) {
	// Preserve invalid explicit values for admission to reject.
	if workflow.Spec.ActiveDeadlineSeconds != nil && *workflow.Spec.ActiveDeadlineSeconds <= 0 {
		return
	}
	seconds := int64(config.ExecutionTimeout(repository, workflow.Spec.ActiveDeadlineSeconds) / time.Second)
	workflow.Spec.ActiveDeadlineSeconds = &seconds
}
