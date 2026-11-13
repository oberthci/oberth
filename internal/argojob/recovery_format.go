package argojob

import (
	"errors"
	"strings"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	corev1 "k8s.io/api/core/v1"
)

// Removed execution formats cannot be resumed by the current server. This is
// a refusal only: it never reconstructs an older command or helper policy.
func validateWorkflowRecovery(workflow *wfv1.Workflow) error {
	if err := validateHelperStorage(workflow); err != nil {
		return err
	}
	var refusal error
	walkTemplates(workflow, func(t *wfv1.Template) {
		templateContainers(t, func(c *corev1.Container) {
			if len(c.Command) > 0 && strings.HasPrefix(c.Command[0], "/run/oberth/usage/") {
				refusal = errors.New("argojob: existing workflow uses a removed server command format; drain old in-flight Workflows before upgrading")
			}
		})
	})
	return refusal
}
