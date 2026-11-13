package argojob

import (
	"context"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	corev1 "k8s.io/api/core/v1"
)

// ownedStepPods joins Argo node IDs to observed Pods. Pod names are opaque;
// namespace, Workflow controller UID and node annotations establish identity.
// Multiple owned Pods for one node leave the binding unknown.
func ownedStepPods(workflow *wfv1.Workflow, pods map[string]*corev1.Pod) map[string]*corev1.Pod {
	result := make(map[string]*corev1.Pod)
	ambiguous := make(map[string]bool)
	for _, pod := range pods {
		if pod == nil {
			continue
		}
		id := pod.Annotations["workflows.argoproj.io/node-id"]
		node, ok := workflow.Status.Nodes[id]
		if !ok || id == "" || node.ID != id || !stepPodIdentityBound(workflow, node, pod) {
			continue
		}
		if result[id] != nil || ambiguous[id] {
			delete(result, id)
			ambiguous[id] = true
			continue
		}
		result[id] = pod
	}
	return result
}

func bindStepPodNames(workflow *wfv1.Workflow, steps []StepResult, pods map[string]*corev1.Pod) {
	owned := ownedStepPods(workflow, pods)
	for i := range steps {
		steps[i].PodName = ""
		if pod := owned[steps[i].NodeID]; pod != nil {
			steps[i].PodName = pod.Name
		}
	}
}

func (controller *Controller) observedStepAttempts(_ context.Context, workflow *wfv1.Workflow) []StepResult {
	steps := stepAttempts(workflow)
	if controller.hub != nil && controller.hub.Healthy() {
		pods := map[string]*corev1.Pod{}
		for _, pod := range controller.hub.PodsOf(workflow.Name) {
			pods[pod.Name] = pod
		}
		bindStepPodNames(workflow, steps, pods)
	}
	return steps
}
