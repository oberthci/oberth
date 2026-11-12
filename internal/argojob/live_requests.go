package argojob

import (
	"context"
	"errors"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/oberthci/oberth/pkg/argoworkflow"
)

// LivePodRequests counts every owned non-terminal Pod, including Pending and
// terminating Pods: their requests remain until Kubernetes removes them.
func (controller *Controller) LivePodRequests(ctx context.Context, name, runID string) (*argoworkflow.PodRequests, error) {
	if controller.kube == nil {
		return nil, errors.New("live CPU requests require Kubernetes observation")
	}
	wf, err := controller.workflows.Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	if err = controller.verifyOwnership(wf, runID); err != nil {
		return nil, err
	}
	if wf.UID == "" {
		return nil, errors.New("live CPU requests require an owned Workflow UID")
	}
	pods, err := controller.kube.CoreV1().Pods(controller.config.Namespace).List(ctx, metav1.ListOptions{LabelSelector: argoWorkflowLabel + "=" + name})
	if err != nil {
		return nil, err
	}
	total := &argoworkflow.PodRequests{}
	for _, pod := range pods.Items {
		owner := metav1.GetControllerOf(&pod)
		if owner == nil || owner.Kind != "Workflow" || owner.UID != wf.UID || pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed {
			continue
		}
		total.CPUMilli += argoworkflow.PodCPURequests(&pod)
		total.MemoryBytes += argoworkflow.PodMemoryRequests(&pod)
		total.Pods++
	}
	return total, nil
}
