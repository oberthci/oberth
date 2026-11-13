package argojob

import (
	"context"
	"io"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
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
