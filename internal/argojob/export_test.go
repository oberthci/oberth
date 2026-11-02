package argojob

import (
	"context"
	"io"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	corev1 "k8s.io/api/core/v1"
)

// publish emits the progress transitions and step logs this observation
// reveals that previous ones did not. It returns an error when the aggregate
// run log budget is breached during step log replay, naming the offending step.
func (controller *Controller) publish(ctx context.Context, workflow *wfv1.Workflow, reporter *progressReporter, destination io.Writer) error {
	return controller.publishLogs(ctx, workflow, reporter, destination, nil)
}

// NewIdentityStore creates a store seeded with the given maps.
func NewIdentityStore(release, ci map[string]PerRepoIdentityConfig) *IdentityStore {
	store := &IdentityStore{}
	store.Replace(release, ci)
	return store
}

func (worker *stepLogFollower) consume(reader io.Reader) error {
	return worker.consumeRecords(reader, false)
}

// Selection trusts the API/kubelet's final incarnation status, not phase alone.
// Historical records grant no authority. The one remaining candidate still
// undergoes every existing live controller and immutable configuration check.
func selectNonrootController(pods []corev1.Pod, namespace string) (*corev1.Pod, error) {
	return selectNonrootControllerWithHistory(pods, namespace, nil)
}

func (c *Controller) sampleResourceUsage(ctx context.Context, wf *wfv1.Workflow, runID string) error {
	return c.sampleResourceUsageWithState(ctx, wf, runID, nil)
}
