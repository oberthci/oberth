package argojob

import (
	"context"
	"errors"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
)

func evictedControllerCandidates(pods []corev1.Pod) map[types.UID]bool {
	candidates := map[types.UID]bool{}
	now := time.Now()
	for i := range pods {
		p := &pods[i]
		if p.Status.Phase == corev1.PodFailed && p.Status.Reason == "Evicted" && !nonrootControllerHistoryIsTerminal(p, now) {
			candidates[p.UID] = true
		}
	}
	return candidates
}

func controllerOwner(p *corev1.Pod) (metav1.OwnerReference, bool) {
	if len(p.OwnerReferences) != 1 {
		return metav1.OwnerReference{}, false
	}
	owner := p.OwnerReferences[0]
	return owner, owner.APIVersion == "apps/v1" && owner.Kind == "ReplicaSet" && owner.Name != "" && owner.UID != "" && owner.Controller != nil && *owner.Controller
}
func sameDeployment(left, right *appsv1.ReplicaSet) bool {
	if len(left.OwnerReferences) != 1 || len(right.OwnerReferences) != 1 {
		return false
	}
	a, b := left.OwnerReferences[0], right.OwnerReferences[0]
	return a.APIVersion == "apps/v1" && a.Kind == "Deployment" && a.Name != "" && a.UID != "" && a.Controller != nil && *a.Controller && b.APIVersion == a.APIVersion && b.Kind == a.Kind && b.Name == a.Name && b.UID == a.UID && b.Controller != nil && *b.Controller
}
func controllerReplicaSet(ctx context.Context, client kubernetes.Interface, namespace string, owner metav1.OwnerReference) (*appsv1.ReplicaSet, error) {
	rs, err := client.AppsV1().ReplicaSets(namespace).Get(ctx, owner.Name, metav1.GetOptions{})
	if err != nil {
		return nil, &profileReadError{message: "argojob: cannot verify controller replacement ReplicaSet", object: namespace + "/ReplicaSet/" + owner.Name, cause: err}
	}
	if rs.Namespace != namespace || rs.Name != owner.Name || rs.UID != owner.UID || rs.ResourceVersion == "" || rs.DeletionTimestamp != nil || rs.Generation < 1 || rs.Status.ObservedGeneration < rs.Generation || rs.Spec.Replicas == nil {
		return nil, errors.New("argojob: controller replacement ReplicaSet identity or generation is unverified")
	}
	return rs, nil
}
func replicaSetSelects(rs *appsv1.ReplicaSet, pod *corev1.Pod) bool {
	selector, err := metav1.LabelSelectorAsSelector(rs.Spec.Selector)
	return err == nil && !selector.Empty() && selector.Matches(labels.Set(pod.Labels))
}

// Eviction can leave missing/Unknown kubelet termination status forever. Use
// positive controller reconciliation evidence instead of inventing an exit:
// one fully verified live replacement, its exact converged ReplicaSet, and
// an older Failed/Evicted Pod bound to that set (or its scaled-zero predecessor
// in the same Deployment). No historical object supplies profile authority.
func verifyEvictedControllerHistory(ctx context.Context, client kubernetes.Interface, namespace string, pods []corev1.Pod, current *corev1.Pod, candidates map[types.UID]bool) error {
	invalid := func() error {
		return errors.New("argojob: evicted controller history is not bound to a verified Ready replacement")
	}
	owner, ok := controllerOwner(current)
	now := time.Now()
	if !ok || current.ResourceVersion == "" || current.CreationTimestamp.IsZero() || current.CreationTimestamp.After(now) || current.CreationTimestamp.After(current.Status.ContainerStatuses[0].State.Running.StartedAt.Time) {
		return invalid()
	}
	liveRS, err := controllerReplicaSet(ctx, client, namespace, owner)
	if err != nil {
		return err
	}
	if *liveRS.Spec.Replicas != 1 || liveRS.Status.Replicas != 1 || liveRS.Status.ReadyReplicas != 1 || !replicaSetSelects(liveRS, current) {
		return errors.New("argojob: controller ReplicaSet has not converged to one Ready replacement")
	}
	sets := map[types.UID]*appsv1.ReplicaSet{liveRS.UID: liveRS}
	for i := range pods {
		p := &pods[i]
		if !candidates[p.UID] {
			continue
		}
		previous, ok := controllerOwner(p)
		if !ok || p.Namespace != namespace || p.ResourceVersion == "" || p.CreationTimestamp.IsZero() || p.CreationTimestamp.After(current.CreationTimestamp.Time) || len(p.Spec.Containers) != 1 || p.Spec.Containers[0].Name != "controller" || p.Spec.Containers[0].Image == "" || len(p.Spec.InitContainers) != 0 || len(p.Spec.EphemeralContainers) != 0 || len(p.Status.InitContainerStatuses) != 0 || len(p.Status.EphemeralContainerStatuses) != 0 || len(p.Status.ContainerStatuses) > 1 {
			return invalid()
		}
		conditions := map[corev1.PodConditionType]bool{}
		for _, condition := range p.Status.Conditions {
			if conditions[condition.Type] {
				return invalid()
			}
			conditions[condition.Type] = true
			if (condition.Type == corev1.PodReady || condition.Type == corev1.ContainersReady) && condition.Status != corev1.ConditionFalse {
				return invalid()
			}
		}
		for _, status := range p.Status.ContainerStatuses {
			if status.Name != "controller" || status.Ready || (status.Started != nil && *status.Started) || status.State.Running != nil || status.LastTerminationState.Running != nil {
				return invalid()
			}
		}
		oldRS := sets[previous.UID]
		if oldRS == nil {
			oldRS, err = controllerReplicaSet(ctx, client, namespace, previous)
			if err != nil {
				return err
			}
			sets[previous.UID] = oldRS
		}
		if oldRS.Name != previous.Name || !replicaSetSelects(oldRS, p) {
			return invalid()
		}
		if oldRS.UID != liveRS.UID && (*oldRS.Spec.Replicas != 0 || oldRS.Status.Replicas != 0 || oldRS.Status.ReadyReplicas != 0 || !sameDeployment(oldRS, liveRS)) {
			return invalid()
		}
	}
	return nil
}
