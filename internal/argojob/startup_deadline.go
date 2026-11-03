package argojob

import (
	"time"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
)

// Startup limits are independent of the workflow execution budget. A long
// build deadline must not let an idle grant monopolize admission for hours.
const workflowStartupTimeout = 10 * time.Minute
const startupObservationTimeout = 2 * time.Minute

type startupDeadlineError string

func (err startupDeadlineError) Error() string               { return string(err) }
func (err startupDeadlineError) InfrastructureFailure() bool { return true }

const noScheduledWorkload = startupDeadlineError("Workflow startup timed out after 10m: no workload Pod scheduled; stopping the Workflow before releasing admission.")
const executorStartupExpired = startupDeadlineError("Workflow startup timed out after 10m: executor identity was not ready; stopping the Workflow before releasing admission.")
const startupObservationExpired = startupDeadlineError("Workflow startup observation unavailable for 2m; stopping the Workflow before releasing admission.")

type workflowStartup struct {
	observingSince   time.Time
	scheduled        bool
	readFailingSince time.Time
	identitySince    map[types.UID]time.Time
}

func (startup *workflowStartup) readFailure(now time.Time) error {
	if startup.readFailingSince.IsZero() {
		startup.readFailingSince = now
	}
	if now.Sub(startup.readFailingSince) >= startupObservationTimeout {
		return startupObservationExpired
	}
	return nil
}

func (startup *workflowStartup) observe(workflow *wfv1.Workflow, pods []corev1.Pod, delivered map[types.UID]bool, now time.Time) error {
	startup.readFailingSince = time.Time{}
	// Retained Argo Pod-node evidence survives Pod GC and server restarts.
	// Workflow/step-group Running alone is not proof of scheduled work.
	for _, node := range workflow.Status.Nodes {
		if node.Type == wfv1.NodeTypePod && (node.HostNodeName != "" || node.Phase == wfv1.NodeSucceeded) {
			startup.scheduled = true
		}
	}
	if startup.identitySince == nil {
		startup.identitySince = make(map[types.UID]time.Time)
	}
	active := make(map[types.UID]bool)
	for i := range pods {
		pod := &pods[i]
		if !workflowOwnsIdentityTarget(workflow, pod) || pod.Labels["oberth.ci/role"] == "executor-identity" {
			continue
		}
		if pod.Spec.NodeName != "" {
			startup.scheduled = true
		}
		for _, condition := range pod.Status.Conditions {
			if condition.Type == corev1.PodScheduled && condition.Status == corev1.ConditionTrue {
				startup.scheduled = true
			}
		}
		_, complete := identityWaiterState(pod)
		if delivered[pod.UID] || complete || pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed {
			continue
		}
		for _, container := range pod.Spec.InitContainers {
			if container.Name != executorTokenWaiter {
				continue
			}
			active[pod.UID] = true
			origin := pod.CreationTimestamp.Time
			if origin.IsZero() {
				origin = startup.identitySince[pod.UID]
				if origin.IsZero() {
					origin = now
				}
			}
			startup.identitySince[pod.UID] = origin
			if now.Sub(origin) >= workflowStartupTimeout {
				return executorStartupExpired
			}
		}
	}
	for uid := range startup.identitySince {
		if !active[uid] {
			delete(startup.identitySince, uid)
		}
	}
	origin := workflow.CreationTimestamp.Time
	if origin.IsZero() {
		origin = startup.observingSince
	}
	if !startup.scheduled && now.Sub(origin) >= workflowStartupTimeout {
		return noScheduledWorkload
	}
	return nil
}
