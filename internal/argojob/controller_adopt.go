package argojob

import (
	"context"
	"fmt"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// A running cache hit needs no live read. A miss or completed cache entry uses
// one confirming live GET. No cached terminal phase can become a run verdict.
func (controller *Controller) workflowForAdoption(ctx context.Context, request AdoptionRequest) (*wfv1.Workflow, bool, error) {
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	var cached *wfv1.Workflow
	if controller.hub != nil {
		if !controller.hub.Healthy() {
			return nil, false, errWatchUnavailable
		}
		if workflow, found := controller.hub.Workflow(request.Name); found {
			if !workflow.Status.Phase.Completed() {
				return workflow, true, nil
			}
			cached = workflow
		}
	}
	workflow, err := controller.workflows.Get(ctx, request.Name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("argojob: read Workflow for adoption: %w", err)
	}
	if cached != nil {
		binding, err := controller.bindAdoption(cached, request)
		if err != nil {
			return nil, false, fmt.Errorf("%w: %w", ErrAdoptionRefused, err)
		}
		if !binding.matches(workflow) {
			return nil, false, fmt.Errorf("%w: Workflow identity changed during startup settlement", ErrAdoptionRefused)
		}
	}
	return workflow, true, nil
}
