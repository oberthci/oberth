package argojob

import (
	"context"
	"errors"
	"fmt"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const workflowCreateCleanupWindow = time.Minute
const workflowCreateCleanupReadBudget = 5 * time.Second

// SettleCreate holds an uncertain submission through a late-commit window.
// No observation before the window closes proves absence. Only a fresh final
// NotFound permits the caller to complete its durable cancellation obligation.
// Found Workflows are ownership checked and UID-fenced, never executed. A failed
// read/delete or an object still present at the end keeps admission held.
func (controller *Controller) SettleCreate(ctx context.Context, name, runID string) error {
	if name == "" || runID == "" {
		return errors.New("argojob: pending create requires Workflow name and run ID")
	}
	until := time.Now().Add(workflowCreateCleanupWindow)
	budgetCtx, cancel := context.WithTimeout(ctx, workflowCreateCleanupWindow+workflowCreateCleanupReadBudget)
	defer cancel()
	for {
		if err := budgetCtx.Err(); err != nil {
			return err
		}
		final := !time.Now().Before(until)
		readCtx, cancelRead := context.WithTimeout(budgetCtx, workflowCreateCleanupReadBudget)
		workflow, err := controller.workflows.Get(readCtx, name, metav1.GetOptions{})
		cancelRead()
		if err == nil {
			if err := controller.verifyOwnership(workflow, runID); err != nil {
				return err
			}
			if workflow.UID == "" {
				return fmt.Errorf("argojob: pending Workflow %s has no UID", name)
			}
			deleteCtx, cancelDelete := context.WithTimeout(budgetCtx, workflowCreateCleanupReadBudget)
			propagation := metav1.DeletePropagationForeground
			// Wait for disappearance, including dependent Pods. Background deletion
			// would release admission while token waiters could still reserve resources.
			err = controller.workflows.Delete(deleteCtx, name, metav1.DeleteOptions{
				PropagationPolicy: &propagation,
				Preconditions:     &metav1.Preconditions{UID: &workflow.UID},
			})
			cancelDelete()
			if err != nil && !apierrors.IsNotFound(err) {
				return fmt.Errorf("argojob: delete pending Workflow %s: %w", name, err)
			}
			if final {
				return fmt.Errorf("argojob: pending Workflow %s still present at final observation", name)
			}
		} else if apierrors.IsNotFound(err) {
			if final {
				return nil
			}
		} else if final || !transientWorkflowCreate(err) {
			return fmt.Errorf("argojob: read pending Workflow %s: %w", name, err)
		}
		delay := min(workflowCreateCleanupReadBudget, time.Until(until))
		if delay <= 0 {
			continue
		}
		timer := time.NewTimer(delay)
		select {
		case <-budgetCtx.Done():
			timer.Stop()
			return budgetCtx.Err()
		case <-timer.C:
		}
	}
}
