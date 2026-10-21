package argoworkflow

import (
	"errors"
	"fmt"
	"strings"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
)

// TrustedTestsEntrypointAnnotation selects only the producers needed by the
// trusted suite. The normal pipeline keeps its own entrypoint and all checks.
const TrustedTestsEntrypointAnnotation = "oberth.ci/trusted-tests-entrypoint"

// TrustedTestsEntrypoint resolves a local producer selection without changing
// the normal entrypoint. Absence preserves the existing full-build preflight.
func TrustedTestsEntrypoint(workflow *wfv1.Workflow) (string, error) {
	if workflow == nil {
		return "", errors.New("argoworkflow: no Workflow to read")
	}
	name, declared := workflow.Annotations[TrustedTestsEntrypointAnnotation]
	if !declared {
		return workflow.Spec.Entrypoint, nil
	}
	if name == "" || strings.TrimSpace(name) != name || strings.ContainsAny(name, "{}") || strings.HasPrefix(name, "oberth-trusted-") {
		return "", fmt.Errorf("argoworkflow: %s requires a literal local producer template", TrustedTestsEntrypointAnnotation)
	}
	for _, template := range workflow.Spec.Templates {
		if template.Name == name {
			return name, nil
		}
	}
	return "", fmt.Errorf("argoworkflow: %s %q names no local template", TrustedTestsEntrypointAnnotation, name)
}
