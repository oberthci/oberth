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

// The sole supported helper policy; older submissions must be drained before upgrade.
const helperStoragePolicy = "argo-256Mi-512Mi-v4"

func validateHelperStorage(wf *wfv1.Workflow) error {
	if wf.Annotations[helperStorageAnnotation] != helperStoragePolicy {
		return fmt.Errorf("argojob: existing workflow has an unsupported helper-storage policy; only %s can be recovered", helperStoragePolicy)
	}
	return nil
}

// Helpers reserve storage and add a limit only when the step declares one.
func helperResources(workflow *wfv1.Workflow, t *wfv1.Template) corev1.ResourceRequirements {
	resources := executorControlResources()
	if !stepDeclaresStorageLimit(workflow, t) {
		delete(resources.Limits, corev1.ResourceEphemeralStorage)
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
