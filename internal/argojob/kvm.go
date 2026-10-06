package argojob

import (
	"errors"
	"fmt"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/oberthci/oberth/pkg/argoworkflow"
)

// kvmDeviceResource is the KubeVirt device plugin resource the server injects.
// Request == limit is mandatory for device-plugin resources in Kubernetes.
const kvmDeviceResource = "devices.kubevirt.io/kvm"

// applyKVMLeaves injects KVM device resources and the OBERTH_KVM=1 env on each
// declared KVM leaf when the server switch is ON. When OFF, it refuses the
// workflow with an infrastructure-class error rather than silently running TCG.
//
// This runs after all other per-template rewrites (workspace mount injection,
// environment injection, nonroot leaves) so it can simply append to the
// already-finalized container.
func applyKVMLeaves(workflow *wfv1.Workflow, config Config) error {
	names, err := argoworkflow.DeclaredKVMTemplates(workflow)
	if err != nil || len(names) == 0 {
		return err
	}

	if !config.KVMEnabled {
		return fmt.Errorf("argojob: workflow declares %s but vm.kvm.enabled is false on this Oberth; "+
			"the server cannot schedule KVM device access", argoworkflow.KVMTemplatesAnnotation)
	}

	nameSet := make(map[string]bool, len(names))
	for _, name := range names {
		nameSet[name] = true
	}

	kvmQuantity := resource.MustParse("1")
	kvmEnv := corev1.EnvVar{Name: "OBERTH_KVM", Value: "1"}

	var applyErr error
	walkTemplates(workflow, func(tmpl *wfv1.Template) {
		if applyErr != nil || !nameSet[tmpl.Name] {
			return
		}

		main := templateMainContainer(tmpl)
		if main == nil {
			applyErr = fmt.Errorf("argojob: KVM template %q has no main container", tmpl.Name)
			return
		}

		// Inject devices.kubevirt.io/kvm: "1" into both requests and limits.
		// Device plugin resources require request == limit in Kubernetes.
		if main.Resources.Requests == nil {
			main.Resources.Requests = corev1.ResourceList{}
		}
		if main.Resources.Limits == nil {
			main.Resources.Limits = corev1.ResourceList{}
		}
		main.Resources.Requests[corev1.ResourceName(kvmDeviceResource)] = kvmQuantity
		main.Resources.Limits[corev1.ResourceName(kvmDeviceResource)] = kvmQuantity

		// Inject OBERTH_KVM=1 so the consumer can assert -accel kvm only
		// (no TCG fallback when this env is present).
		main.Env = overrideEnvironment(main.Env, []corev1.EnvVar{kvmEnv})
	})

	return applyErr
}

// ErrKVMNotEnabled is the sentinel for the infrastructure-class failure when
// KVM leaves are declared but the server switch is off.
var ErrKVMNotEnabled = errors.New("argojob: KVM leaves declared but vm.kvm.enabled is false")
