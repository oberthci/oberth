package argojob

import (
	"context"
	"errors"
	"fmt"
	"strings"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"

	"github.com/oberthci/oberth/pkg/argoworkflow"
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
	// Without the Workflow UID, labels and names cannot prove ownership.
	return nil
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
	return controller.cleanupVMIsForRun(ctx, workflow)
}

func (controller *Controller) cleanupVMIsForRun(ctx context.Context, workflow *wfv1.Workflow) error {
	workflowName := workflow.Name
	selector := "oberth.ci/workflow=" + workflowName
	client := controller.vmi.Resource(vmiResource).Namespace(controller.config.Namespace)
	objects, err := client.List(ctx, metav1.ListOptions{LabelSelector: selector})
	if apierrors.IsNotFound(err) {
		return nil
	} // KubeVirt CRD is absent.
	if err != nil {
		return fmt.Errorf("argojob: list run VMIs: %w", err)
	}
	known := map[string]types.UID{}
	var failures []error
	if _, declared := workflow.Annotations[argoworkflow.CleanupVerdictAnnotation]; declared {
		var identityErr error
		known, identityErr = controller.cleanupIdentities(ctx, workflow)
		if identityErr != nil {
			var pending *cleanupPendingIdentityError
			if !errors.As(identityErr, &pending) {
				return identityErr
			}
			failures = append(failures, identityErr)
		}
	}
	for _, object := range objects.Items {
		if object.GetLabels()["oberth.ci/workflow"] != workflowName || !strings.HasPrefix(object.GetName(), workflowName+"-") || !workflowOwnsResource(workflow, &object) {
			continue
		}
		if original := known[cleanupKey(vmiResource, object.GetName())]; original != "" && original != object.GetUID() {
			failures = append(failures, fmt.Errorf("argojob: replacement VMI %s; original UID differs", object.GetName()))
			continue
		}
		uid := object.GetUID()
		if uid == "" {
			failures = append(failures, fmt.Errorf("argojob: VMI %s has no UID", object.GetName()))
			continue
		}
		err = client.Delete(ctx, object.GetName(), metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &uid}})
		if err != nil && !apierrors.IsNotFound(err) {
			failures = append(failures, fmt.Errorf("argojob: delete leftover VMI %s: %w", object.GetName(), err))
		}
	}
	return errors.Join(failures...)
}
