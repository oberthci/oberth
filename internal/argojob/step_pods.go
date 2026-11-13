package argojob

import (
	"context"
	"time"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
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

// stepLogPodSnapshot pages through the native Pod list without the diagnostic
// snapshot's task cap. The read context bounds observation time, not run size.
func (controller *Controller) stepLogPodSnapshot(ctx context.Context, workflow *wfv1.Workflow) (map[string]*corev1.Pod, bool) {
	if controller.kube == nil {
		return nil, false
	}
	pods := map[string]*corev1.Pod{}
	cursors := map[string]bool{}
	cursor, version := "", ""
	for {
		list, err := controller.kube.CoreV1().Pods(workflow.Namespace).List(ctx, metav1.ListOptions{
			LabelSelector: labels.Set{"workflows.argoproj.io/workflow": workflow.Name}.AsSelector().String(),
			Limit:         128, Continue: cursor,
		})
		if err != nil || (cursor != "" && version != list.ResourceVersion) {
			return nil, false
		}
		version = list.ResourceVersion
		for i := range list.Items {
			pod := &list.Items[i]
			if pods[pod.Name] != nil {
				return nil, false
			}
			pods[pod.Name] = pod
		}
		if list.Continue == "" {
			return pods, true
		}
		if version == "" || cursors[list.Continue] || ctx.Err() != nil {
			return nil, false
		}
		cursors[list.Continue] = true
		cursor = list.Continue
	}
}

func (controller *Controller) observedStepAttempts(ctx context.Context, workflow *wfv1.Workflow) []StepResult {
	steps := stepAttempts(workflow)
	if len(steps) == 0 {
		return steps
	}
	budget, attempts := time.Second, 1
	if workflow.Status.Phase.Completed() {
		budget, attempts = finalLogDrainBudget, 3
	}
	readCtx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	for attempt := 0; attempt < attempts; attempt++ {
		attemptCtx, cancelAttempt := context.WithTimeout(readCtx, finalLogReadTimeout)
		pods, complete := controller.stepLogPodSnapshot(attemptCtx, workflow)
		cancelAttempt()
		if complete {
			bindStepPodNames(workflow, steps, pods)
			bound := true
			for _, step := range steps {
				if workflow.Status.Nodes[step.NodeID].Type == wfv1.NodeTypePod && step.PodName == "" {
					bound = false
				}
			}
			if bound {
				return steps
			}
		}
		if attempt+1 < attempts {
			timer := time.NewTimer(time.Duration(attempt+1) * 250 * time.Millisecond)
			select {
			case <-readCtx.Done():
				timer.Stop()
				return steps
			case <-timer.C:
			}
		}
	}
	return steps
}
