package argojob

import (
	"context"
	"errors"
	"fmt"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const workflowCreateCleanupReadBudget = 5 * time.Second
const workflowCreateCleanupAttemptBudget = 10 * time.Second

// SettleCreate performs one bounded observation, without waiting or retrying.
// The background owner supplies the durable late-commit boundary. A pending
// Stop, conflict or API failure keeps the grant until Argo finishes.
func (controller *Controller) SettleCreate(ctx context.Context, name, runID string, notBefore time.Time) (bool, error) {
	if name == "" || runID == "" || notBefore.IsZero() {
		return false, errors.New("argojob: pending create requires name, run ID and observation boundary")
	}
	budgetCtx, cancel := context.WithTimeout(ctx, workflowCreateCleanupAttemptBudget)
	defer cancel()
	finalObservation := !time.Now().Before(notBefore)
	readCtx, cancelRead := context.WithTimeout(budgetCtx, workflowCreateCleanupReadBudget)
	workflow, err := controller.workflows.Get(readCtx, name, metav1.GetOptions{})
	readErr := readCtx.Err()
	cancelRead()
	if readErr != nil {
		return false, readErr
	}
	if err != nil {
		if apierrors.IsNotFound(err) {
			return finalObservation && budgetCtx.Err() == nil, budgetCtx.Err()
		}
		return false, fmt.Errorf("argojob: read pending Workflow %s: %w", name, err)
	}
	if err := controller.verifyOwnership(workflow, runID); err != nil {
		return false, err
	}
	if workflow.UID == "" {
		return false, fmt.Errorf("argojob: pending Workflow %s has no UID", name)
	}
	if workflow.DeletionTimestamp != nil {
		return false, nil
	}
	if workflow.Status.Phase.Completed() {
		return true, nil
	}
	stopCtx, cancelStop := context.WithTimeout(budgetCtx, workflowCreateCleanupReadBudget)
	defer cancelStop()
	_, err = requestWorkflowStop(stopCtx, controller.workflows, workflow)
	if err != nil {
		return false, fmt.Errorf("argojob: Stop pending Workflow %s: %w", name, err)
	}
	return false, nil // Stop acceptance is not terminal evidence; onExit can run.
}
