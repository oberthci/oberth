package argoworkflow

import (
	"errors"
	"strings"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
)

// ErrWorkflowCapabilityRequired also explains refusal of historical execution declarations.
var ErrWorkflowCapabilityRequired = errors.New("removed or ungranted execution feature; migrate to a workflow capability grant with allowlisted resource kinds and extended resources")

// RefuseRetiredWorkflow prevents old server-owned execution annotations from becoming inert hints.
func RefuseRetiredWorkflow(w *wfv1.Workflow) error {
	for _, name := range []string{"oberth.ci/kvm-templates", "oberth.ci/guest-test-templates", "oberth.ci/trusted-tests-entrypoint", "oberth.ci/cleanup-verdict", "oberth.ci/trusted-kvm-intent", "oberth.ci/vmi-quota"} {
		if _, ok := w.Annotations[name]; ok {
			return ErrWorkflowCapabilityRequired
		}
	}
	return nil
}

// RetiredValue accepts historical chart values solely so upgrades can ignore them.
func RetiredValue(key string) bool {
	for _, prefix := range []string{"trustedTests", "vm", "argo.vmLauncherServiceAccount", "argo.containerDiskPrefixes", "argo.kubevirtCeiling"} {
		if key == prefix || strings.HasPrefix(key, prefix+".") || strings.HasPrefix(key, prefix+"[") {
			return true
		}
	}
	return false
}
