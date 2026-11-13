package argojob

import (
	"fmt"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/oberthci/oberth/pkg/argoworkflow"
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

// v2 applies the same budgets to nonroot producer helpers too. Every helper carries
// the 512Mi limit, so a step without its own limit gets a 512Mi Pod total.
const cappedHelperStoragePolicy = "argo-256Mi-512Mi-v2"

// v3 keeps the v2 helpers and requests. The kubelet Pod total sums only the
// limits that are declared, so a helper limit is added only when the step's own
// containers declare an ephemeral-storage limit (#1491). Its decision counted
// template-default container-set limits, which Argo replaces, and missed
// default-only and restartable-init sidecars. It stays for exact recovery.
const stepLimitHelperStoragePolicy = "argo-256Mi-512Mi-v3"

// v4 decides "declared" from the containers Argo actually runs after merging
// spec.templateDefaults (stepStorageLimits).
const helperStoragePolicy = "argo-256Mi-512Mi-v4"

// Reconstruct the immutable resources of already-submitted Workflows. This
// switch is private to recovery; authored annotations cannot opt new runs out.
func recoverHelperStorage(wf *wfv1.Workflow, request *Request) error {
	policy := wf.Annotations[helperStorageAnnotation]
	switch policy {
	case "", legacyHelperStoragePolicy, cappedHelperStoragePolicy, stepLimitHelperStoragePolicy, helperStoragePolicy:
	default:
		return fmt.Errorf("argojob: existing workflow has an unknown helper-storage policy")
	}
	request.legacyHelperStorage = policy == ""
	request.legacyProfileHelperResources = policy == "" || policy == legacyHelperStoragePolicy
	request.legacyHelperStorageLimits = policy == cappedHelperStoragePolicy
	request.legacyStepLimitHelperStorage = policy == stepLimitHelperStoragePolicy
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
	case request.legacyStepLimitHelperStorage:
		return stepLimitHelperStoragePolicy
	}
	return helperStoragePolicy
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
// waiter, wait, or a resource/data executor main) in template t. Since v3 a
// helper reserves storage but adds a limit only when the step itself declares
// one: an undeclared step must not inherit a Pod-level cap.
func helperResources(workflow *wfv1.Workflow, t *wfv1.Template, storage bool) corev1.ResourceRequirements {
	resources := executorResourcesWithStorage(storage)
	if !storage {
		return resources
	}
	switch workflow.Annotations[helperStorageAnnotation] {
	case stepLimitHelperStoragePolicy:
		if !stepDeclaresStorageLimitV3(workflow, t) {
			delete(resources.Limits, corev1.ResourceEphemeralStorage)
		}
	case helperStoragePolicy:
		if !stepDeclaresStorageLimit(workflow, t) {
			delete(resources.Limits, corev1.ResourceEphemeralStorage)
		}
	}
	return resources
}

// stepDeclaresStorageLimit reports whether the step's own containers add an
// ephemeral-storage limit to the kubelet Pod total.
func stepDeclaresStorageLimit(workflow *wfv1.Workflow, t *wfv1.Template) bool {
	return len(stepStorageLimits(workflow, t)) > 0
}

// stepStorageLimits returns, by container name, the positive ephemeral-storage
// limits of the containers the step contributes to the kubelet Pod sum, as Argo
// v4.0.8 runs them after merging spec.templateDefaults:
//   - the "main" container of a container or script template, whose fields
//     merge with the default of the same kind;
//   - container-set members, whose list replaces the default list;
//   - sidecars and restartable (native sidecar) init containers, which merge by
//     name, so a default-only entry runs too.
//
// One-shot init containers enter the Pod total by maximum, not by sum, and keep
// their own effect. Server-owned helpers are never step containers.
func stepStorageLimits(workflow *wfv1.Workflow, t *wfv1.Template) map[string]resource.Quantity {
	limits := map[string]resource.Quantity{}
	if t == nil {
		return limits
	}
	add := func(name string, c, defaults *corev1.Container) {
		q, ok := c.Resources.Limits[corev1.ResourceEphemeralStorage]
		if !ok && defaults != nil {
			q, ok = defaults.Resources.Limits[corev1.ResourceEphemeralStorage]
		}
		if ok && q.Sign() > 0 {
			limits[name] = q
		}
	}
	defaults := workflow.Spec.TemplateDefaults
	if defaults == nil {
		defaults = &wfv1.Template{}
	}
	switch {
	case t.Container != nil:
		add("main", t.Container, defaults.Container)
	case t.Script != nil:
		var inherited *corev1.Container
		if defaults.Script != nil {
			inherited = &defaults.Script.Container
		}
		add("main", &t.Script.Container, inherited)
	case t.ContainerSet != nil:
		for i := range t.ContainerSet.Containers {
			c := &t.ContainerSet.Containers[i].Container
			add(c.Name, c, nil)
		}
	}
	argoworkflow.EachTemplateHelper(t.Sidecars, defaults.Sidecars, func(c, inherited *corev1.Container) {
		add(c.Name, c, inherited)
	})
	argoworkflow.EachTemplateHelper(t.InitContainers, defaults.InitContainers, func(c, inherited *corev1.Container) {
		restart := c.RestartPolicy
		if restart == nil && inherited != nil {
			restart = inherited.RestartPolicy
		}
		if restart != nil && *restart == corev1.ContainerRestartPolicyAlways {
			add(c.Name, c, inherited)
		}
	})
	return limits
}

// stepDeclaresStorageLimitV3 is the v3 decision, kept byte-for-byte so that
// adopted v3 Workflows rebuild with the helper limits they were submitted with.
// New Workflows use stepDeclaresStorageLimit.
func stepDeclaresStorageLimitV3(workflow *wfv1.Workflow, t *wfv1.Template) bool {
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
