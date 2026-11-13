package argojob

import (
	"fmt"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/oberthci/oberth/pkg/argoworkflow"
)

const helperStorageAnnotation = "oberth.ci/helper-storage"

// The sole supported helper policy; older submissions must be drained before upgrade.
const helperStoragePolicy = "argo-256Mi-512Mi-v4"

func validateHelperStorage(wf *wfv1.Workflow) error {
	if wf.Annotations[helperStorageAnnotation] != helperStoragePolicy {
		return fmt.Errorf("argojob: existing workflow has an unsupported helper-storage policy; only %s can be recovered", helperStoragePolicy)
	}
	return nil
}

// The Oberth token waiter reserves storage and adds a limit only when the step
// declares one. Argo-generated executors use native controller resources.
func helperResources(workflow *wfv1.Workflow, t *wfv1.Template) corev1.ResourceRequirements {
	resources := argoworkflow.ExecutorResources()
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
