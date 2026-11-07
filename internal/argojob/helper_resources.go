package argojob

import (
	"fmt"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

// CPU/memory remain provisional. The pinned Argo v4.0.8 executor occupies
// 160825009 bytes in the shared disk emptyDir before workload scratch/logs.
// Reserve helper-owned storage in the Pod sum; init limits alone do not raise
// the steady-state Pod allowance. Headroom is provisional, not a measured peak.
// The limit is added only where the step declares its own (see helperResources).
func executorControlResources() corev1.ResourceRequirements {
	return corev1.ResourceRequirements{
		Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("50m"), corev1.ResourceMemory: resource.MustParse("64Mi"), corev1.ResourceEphemeralStorage: resource.MustParse("256Mi")},
		Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("500m"), corev1.ResourceMemory: resource.MustParse("512Mi"), corev1.ResourceEphemeralStorage: resource.MustParse("512Mi")},
	}
}

const helperStorageAnnotation = "oberth.ci/helper-storage"
const legacyHelperStoragePolicy = "argo-256Mi-512Mi-v1"

// v2 applies the same budgets to verified named-profile producer helpers too.
// The controller profile and its proof remain unchanged. Every helper carries
// the 512Mi limit, so a step without its own limit gets a 512Mi Pod total.
const cappedHelperStoragePolicy = "argo-256Mi-512Mi-v2"

// v3 keeps the v2 helpers and requests. The kubelet Pod total sums only the
// limits that are declared, so a helper limit is added only when the step's own
// containers declare an ephemeral-storage limit (#1491).
const helperStoragePolicy = "argo-256Mi-512Mi-v3"

// Reconstruct the immutable resources of already-submitted Workflows. This
// switch is private to recovery; authored annotations cannot opt new runs out.
func recoverHelperStorage(wf *wfv1.Workflow, request *Request) error {
	policy := wf.Annotations[helperStorageAnnotation]
	switch policy {
	case "", legacyHelperStoragePolicy, cappedHelperStoragePolicy, helperStoragePolicy:
	default:
		return fmt.Errorf("argojob: existing workflow has an unknown helper-storage policy")
	}
	request.legacyHelperStorage = policy == ""
	request.legacyProfileHelperResources = policy == "" || policy == legacyHelperStoragePolicy
	request.legacyHelperStorageLimits = policy == cappedHelperStoragePolicy
	return nil
}

// The policy Build stamps for a request; recovery selects a historical one.
func (request Request) helperStoragePolicy() string {
	switch {
	case request.legacyHelperStorage:
		return ""
	case request.legacyProfileHelperResources:
		return legacyHelperStoragePolicy
	case request.legacyHelperStorageLimits:
		return cappedHelperStoragePolicy
	}
	return helperStoragePolicy
}

// Named-profile producer helpers carry budgets since v2.
func profileHelperBudgets(workflow *wfv1.Workflow) bool {
	policy := workflow.Annotations[helperStorageAnnotation]
	return policy == cappedHelperStoragePolicy || policy == helperStoragePolicy
}

func executorResourcesWithStorage(enabled bool) corev1.ResourceRequirements {
	resources := executorControlResources()
	if !enabled {
		delete(resources.Requests, corev1.ResourceEphemeralStorage)
		delete(resources.Limits, corev1.ResourceEphemeralStorage)
	}
	return resources
}

// helperResources is the budget of one server-owned helper (init, the token
// waiter, wait, or a resource/data executor main) in template t. Under the v3
// policy a helper reserves storage but adds a limit only when the step itself
// declares one: an undeclared step must not inherit a Pod-level cap.
func helperResources(workflow *wfv1.Workflow, t *wfv1.Template, storage bool) corev1.ResourceRequirements {
	resources := executorResourcesWithStorage(storage)
	if storage && workflow.Annotations[helperStorageAnnotation] == helperStoragePolicy && !stepDeclaresStorageLimit(workflow, t) {
		delete(resources.Limits, corev1.ResourceEphemeralStorage)
	}
	return resources
}

// stepDeclaresStorageLimit reports whether the step's own regular containers,
// directly or through spec.templateDefaults, declare an ephemeral-storage
// limit. Those are the limits the kubelet sums into the Pod total. Init
// containers count by maximum, not by sum, and keep their own effect.
func stepDeclaresStorageLimit(workflow *wfv1.Workflow, t *wfv1.Template) bool {
	if t == nil {
		return false
	}
	limited := func(c *corev1.Container) bool {
		if c == nil {
			return false
		}
		q, ok := c.Resources.Limits[corev1.ResourceEphemeralStorage]
		return ok && q.Sign() > 0
	}
	defaults := workflow.Spec.TemplateDefaults
	if t.Container != nil && (limited(t.Container) || defaults != nil && limited(defaults.Container)) {
		return true
	}
	if t.Script != nil && (limited(&t.Script.Container) || defaults != nil && defaults.Script != nil && limited(&defaults.Script.Container)) {
		return true
	}
	if t.ContainerSet != nil {
		for i := range t.ContainerSet.Containers {
			if limited(&t.ContainerSet.Containers[i].Container) {
				return true
			}
			if defaults != nil && defaults.ContainerSet != nil {
				for j := range defaults.ContainerSet.Containers {
					if defaults.ContainerSet.Containers[j].Name == t.ContainerSet.Containers[i].Name && limited(&defaults.ContainerSet.Containers[j].Container) {
						return true
					}
				}
			}
		}
	}
	for i := range t.Sidecars {
		if limited(&t.Sidecars[i].Container) {
			return true
		}
		if defaults != nil {
			for j := range defaults.Sidecars {
				if defaults.Sidecars[j].Name == t.Sidecars[i].Name && limited(&defaults.Sidecars[j].Container) {
					return true
				}
			}
		}
	}
	return false
}
