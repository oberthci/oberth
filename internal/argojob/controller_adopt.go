package argojob

import (
	"context"
	"errors"
	"fmt"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ErrAdoptionUnavailable marks a shared watch outage; callers may wait for it.
var ErrAdoptionUnavailable = errors.New("argojob: adoption observation unavailable")

// Cache hits bind identity only. WaitResumed owns live completion settlement,
// log publication and executor identity retirement. A miss confirms with GET.
func (controller *Controller) workflowForAdoption(ctx context.Context, request AdoptionRequest) (*wfv1.Workflow, bool, error) {
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	if controller.hub != nil {
		if !controller.hub.Healthy() {
			return nil, false, fmt.Errorf("%w: %w", ErrAdoptionUnavailable, errWatchUnavailable)
		}
		if workflow, found := controller.hub.Workflow(request.Name); found {
			return workflow, true, nil
		}
	}
	workflow, err := controller.workflows.Get(ctx, request.Name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("argojob: read Workflow for adoption: %w", err)
	}
	return workflow, true, nil
}
