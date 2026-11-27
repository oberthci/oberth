package installer

import (
	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	resources "k8s.io/component-helpers/resource"
	scheduling "k8s.io/component-helpers/scheduling/corev1"
	"k8s.io/component-helpers/scheduling/corev1/nodeaffinity"
)

// Vectors are CPU millicores, memory bytes, ephemeral bytes, Pods, memory-limit
// bytes and ephemeral-limit bytes. Bounds keep all sums and integer division exact.
const capacityMax = int64(1 << 50)

func capacityBudget(e CapacityEvidence) ([6]int64, bool) {
	var budget [6]int64
	if !capacityEvidenceComplete(e) || !capacityNodeReady(e.Node) {
		return budget, false
	}
	budget, ok := capacityQuotaBudget(e.Quotas)
	if !ok {
		return budget, false
	}
	available, ok := capacityAvailable(e)
	if !ok {
		return budget, false
	}
	for d := range 4 {
		budget[d] = min(budget[d], available[d])
		if budget[d] <= 0 {
			return budget, false
		}
	}
	scratch, ok := capacityQuantity(e.OperatingScratch, false)
	if !ok || scratch <= 0 {
		return budget, false
	}
	budget[2] = min(budget[2], scratch)
	budget[4] = min(budget[4], budget[1])
	budget[5] = min(budget[5], budget[2])
	return budget, true
}

func capacityQuotaBudget(quotas []corev1.ResourceQuota) ([6]int64, bool) {
	var budget [6]int64
	seen := map[string]bool{}
	for _, quota := range quotas {
		if quota.Namespace == "" || seen[quota.Namespace] || len(quota.Spec.Scopes) > 0 || quota.Spec.ScopeSelector != nil {
			return budget, false
		}
		seen[quota.Namespace] = true
		part, ok := quotaVector(quota.Spec.Hard)
		if !ok || !addCapacity(&budget, part) {
			return budget, false
		}
	}
	return budget, true
}

func capacityAvailable(e CapacityEvidence) ([6]int64, bool) {
	available, ok := allocationVector(e.Node.Status.Allocatable)
	if !ok {
		return available, false
	}
	for _, pod := range e.NonRunAndMaintenance {
		for _, quota := range e.Quotas {
			if quota.Namespace == pod.Namespace {
				return available, false
			}
		}
		part, valid := podVector(pod, false)
		if !valid {
			return available, false
		}
		for d := range 4 {
			if available[d] < part[d] {
				return available, false
			}
			available[d] -= part[d]
		}
	}
	return available, true
}

func capacityEvidenceComplete(e CapacityEvidence) bool {
	if e.Snapshot == "" || e.CapturedAt.IsZero() || e.CurrentCap < 1 || (e.Origin != "chosen" && e.Origin != "default") || len(e.NonRunAndMaintenance) == 0 || len(e.Quotas) == 0 {
		return false
	}
	for _, name := range []string{"G1", "G2", "G3", "G7", "placement"} {
		if e.Receipts[name] == "" {
			return false
		}
	}
	return true
}

func capacityNodeReady(node corev1.Node) bool {
	if node.Spec.Unschedulable {
		return false
	}
	ready := false
	pressure := map[corev1.NodeConditionType]bool{}
	for _, condition := range node.Status.Conditions {
		switch condition.Type {
		case corev1.NodeReady:
			ready = condition.Status == corev1.ConditionTrue
		case corev1.NodeMemoryPressure, corev1.NodeDiskPressure, corev1.NodePIDPressure:
			if condition.Status != corev1.ConditionFalse {
				return false
			}
			pressure[condition.Type] = true
		}
	}
	return ready && len(pressure) == 3
}

func allocationVector(list corev1.ResourceList) ([6]int64, bool) {
	var vector [6]int64
	for d, name := range []corev1.ResourceName{corev1.ResourceCPU, corev1.ResourceMemory, corev1.ResourceEphemeralStorage, corev1.ResourcePods} {
		quantity, found := list[name]
		n, ok := capacityQuantity(quantity, d == 0)
		if !found || !ok {
			return vector, false
		}
		vector[d] = n
	}
	return vector, true
}

func quotaVector(list corev1.ResourceList) ([6]int64, bool) {
	var vector [6]int64
	for d, name := range []corev1.ResourceName{corev1.ResourceRequestsCPU, corev1.ResourceRequestsMemory, corev1.ResourceRequestsEphemeralStorage, corev1.ResourcePods, corev1.ResourceLimitsMemory, corev1.ResourceLimitsEphemeralStorage} {
		quantity, found := list[name]
		n, ok := capacityQuantity(quantity, d == 0)
		if !found || !ok || n <= 0 {
			return vector, false
		}
		vector[d] = n
	}
	return vector, true
}

func runVector(e CapacityEvidence, pods []corev1.Pod) ([6]int64, bool) {
	var vector [6]int64
	if len(pods) == 0 {
		return vector, false
	}
	seen := map[string]bool{}
	for _, pod := range pods {
		id := string(pod.UID)
		if id == "" || seen[id] || !capacityPlacement(e.Node, pod) {
			return vector, false
		}
		seen[id] = true
		covered := false
		for _, quota := range e.Quotas {
			covered = covered || quota.Namespace == pod.Namespace
		}
		part, ok := podVector(pod, true)
		if !covered || !ok || !addCapacity(&vector, part) {
			return vector, false
		}
	}
	return vector, true
}

func podVector(pod corev1.Pod, run bool) ([6]int64, bool) {
	var vector [6]int64
	if len(pod.Spec.Containers) == 0 || pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed {
		return vector, false
	}
	for _, container := range append(append([]corev1.Container{}, pod.Spec.Containers...), pod.Spec.InitContainers...) {
		if !capacityContainer(container.Resources, run) {
			return vector, false
		}
	}
	requests := resources.PodRequests(&pod, resources.PodResourcesOptions{})
	limits := resources.PodLimits(&pod, resources.PodResourcesOptions{})
	for d, name := range []corev1.ResourceName{corev1.ResourceCPU, corev1.ResourceMemory, corev1.ResourceEphemeralStorage} {
		n, ok := capacityQuantity(requests[name], d == 0)
		if !ok {
			return vector, false
		}
		vector[d] = n
	}
	vector[3] = 1
	for d, name := range []corev1.ResourceName{corev1.ResourceMemory, corev1.ResourceEphemeralStorage} {
		n, ok := capacityQuantity(limits[name], false)
		if !ok {
			return vector, false
		}
		vector[4+d] = n
	}
	return vector, true
}

func capacityContainer(r corev1.ResourceRequirements, run bool) bool {
	for _, list := range []corev1.ResourceList{r.Requests, r.Limits} {
		for name, q := range list {
			if _, ok := capacityQuantity(q, name == corev1.ResourceCPU); !ok || (!resourceChoiceName(name) && q.Sign() > 0) {
				return false
			}
		}
	}
	for _, name := range []corev1.ResourceName{corev1.ResourceCPU, corev1.ResourceMemory, corev1.ResourceEphemeralStorage} {
		if !capacityRequest(r, name, run) {
			return false
		}
	}
	return len(r.Claims) == 0
}

func capacityRequest(r corev1.ResourceRequirements, name corev1.ResourceName, run bool) bool {
	request, present := r.Requests[name]
	if !present && (run || name != corev1.ResourceEphemeralStorage) {
		return false
	}
	limit, limited := r.Limits[name]
	if limited && request.Cmp(limit) > 0 {
		return false
	}
	if run && name != corev1.ResourceCPU {
		return limited && limit.Sign() > 0
	}
	return true
}

func capacityPlacement(node corev1.Node, pod corev1.Pod) bool {
	if !capacityPlacementKnown(pod) {
		return false
	}
	if pod.Spec.NodeName != "" && pod.Spec.NodeName != node.Name {
		return false
	}
	match, err := nodeaffinity.GetRequiredNodeAffinity(&pod).Match(&node)
	_, untolerated := scheduling.FindMatchingUntoleratedTaint(logr.Discard(), node.Spec.Taints, pod.Spec.Tolerations, func(t *corev1.Taint) bool {
		return t.Effect == corev1.TaintEffectNoSchedule || t.Effect == corev1.TaintEffectNoExecute
	}, false)
	return match && err == nil && !untolerated
}

func capacityPlacementKnown(pod corev1.Pod) bool {
	if len(pod.Spec.ResourceClaims) > 0 || len(pod.Spec.TopologySpreadConstraints) > 0 || len(pod.Spec.SchedulingGates) > 0 {
		return false
	}
	affinity := pod.Spec.Affinity
	return affinity == nil || (affinity.PodAffinity == nil && affinity.PodAntiAffinity == nil)
}

func capacityQuantity(q resource.Quantity, cpu bool) (int64, bool) {
	maximum := *resource.NewQuantity(capacityMax, resource.DecimalSI)
	if cpu {
		maximum = *resource.NewMilliQuantity(capacityMax, resource.DecimalSI)
	}
	if q.Sign() < 0 || q.Cmp(maximum) > 0 {
		return 0, false
	}
	if cpu {
		return q.MilliValue(), true
	}
	return q.Value(), true
}

func addCapacity(total *[6]int64, part [6]int64) bool {
	for d, n := range part {
		if n < 0 || total[d] > capacityMax-n {
			return false
		}
		total[d] += n
	}
	return true
}

func capacityNamespaceFit(e CapacityEvidence, cap int64) bool {
	for _, quota := range e.Quotas {
		budget, ok := quotaVector(quota.Spec.Hard)
		if !ok {
			return false
		}
		for _, family := range e.Families {
			vector, valid := capacityNamespaceVector(family.WorstPods, quota.Namespace)
			if !valid {
				return false
			}
			if !capacityVectorFit(budget, vector, cap) {
				return false
			}
			// Aggregate worst coverage does not imply namespace coverage when
			// phases move between namespaces. Require each measured phase to
			// fit this namespace's declared worst vector, which fits its quota.
			for _, interval := range family.Intervals {
				phase, valid := capacityNamespaceVector(interval.Pods, quota.Namespace)
				if !valid || !capacityVectorFit(vector, phase, 1) {
					return false
				}
			}
		}
	}
	return true
}

func capacityNamespaceVector(pods []corev1.Pod, namespace string) ([6]int64, bool) {
	var vector [6]int64
	for _, pod := range pods {
		if pod.Namespace != namespace {
			continue
		}
		part, ok := podVector(pod, true)
		if !ok || !addCapacity(&vector, part) {
			return vector, false
		}
	}
	return vector, true
}
