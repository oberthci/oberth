package argojob

import (
	"context"
	"io"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
)

// publish drives the production status observer without opening Pod logs.
func (controller *Controller) publish(ctx context.Context, workflow *wfv1.Workflow, reporter *progressReporter, destination io.Writer) error {
	controller.observeStepProgress(ctx, workflow, reporter, destination)
	return nil
}

// NewIdentityStore creates a store seeded with the given maps.
func NewIdentityStore(release, ci map[string]PerRepoIdentityConfig) *IdentityStore {
	store := &IdentityStore{}
	store.Replace(release, ci)
	return store
}
