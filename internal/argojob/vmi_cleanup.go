package argojob

import (
	"context"
	"fmt"
	"strings"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

var vmiResource = schema.GroupVersionResource{Group: "kubevirt.io", Version: "v1", Resource: "virtualmachineinstances"}

// CleanupVMIs removes admitted VMIs that outlived their Workflow. Admission
// pins the label and name prefix, and deletion pins each observed UID.
func (controller *Controller) CleanupVMIs(ctx context.Context, workflowName, runID string) error {
	if controller.vmi == nil {
		return nil
	}
	if workflowName == "" || runID == "" {
		return fmt.Errorf("argojob: Workflow name and run ID are required for VMI cleanup")
	}
	workflow, err := controller.workflows.Get(ctx, workflowName, metav1.GetOptions{})
	missingWorkflow := apierrors.IsNotFound(err)
	if err == nil {
		return controller.cleanupVMIsForWorkflow(ctx, workflow, runID)
	} else if !missingWorkflow {
		return fmt.Errorf("argojob: read Workflow before VMI cleanup: %w", err)
	}
	return controller.cleanupVMIsForRun(ctx, workflowName, true)
}

// cleanupVMIsForWorkflow reuses the Workflow already observed by Wait or
// TerminalState, avoiding another read during terminalization.
func (controller *Controller) cleanupVMIsForWorkflow(ctx context.Context, workflow *wfv1.Workflow, runID string) error {
	if controller.vmi == nil {
		return nil
	}
	if err := controller.verifyOwnership(workflow, runID); err != nil {
		return err
	}
	if !hasVMIResources(workflow) {
		return nil
	}
	return controller.cleanupVMIsForRun(ctx, workflow.Name, false)
}

func (controller *Controller) cleanupVMIsForRun(ctx context.Context, workflowName string, missingWorkflow bool) error {
	selector := "oberth.ci/workflow=" + workflowName
	client := controller.vmi.Resource(vmiResource).Namespace(controller.config.Namespace)
	objects, err := client.List(ctx, metav1.ListOptions{LabelSelector: selector})
	if apierrors.IsNotFound(err) {
		return nil
	} // KubeVirt CRD is absent.
	if missingWorkflow && apierrors.IsForbidden(err) {
		// An ordinary cancelled run may never have had VMI access. Its
		// missing Workflow cannot distinguish that case, so do not block it.
		return nil
	}
	if err != nil {
		return fmt.Errorf("argojob: list run VMIs: %w", err)
	}
	for _, object := range objects.Items {
		if object.GetLabels()["oberth.ci/workflow"] != workflowName || !strings.HasPrefix(object.GetName(), workflowName+"-") {
			continue
		}
		uid := object.GetUID()
		if uid == "" {
			return fmt.Errorf("argojob: VMI %s has no UID", object.GetName())
		}
		err = client.Delete(ctx, object.GetName(), metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &uid}})
		if err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("argojob: delete leftover VMI %s: %w", object.GetName(), err)
		}
	}
	return nil
}
