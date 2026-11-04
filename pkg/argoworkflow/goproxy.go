package argoworkflow

import (
	"fmt"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
)

const GoProxyAnnotation = "oberth.ci/goproxy"
const GoProxyMirrorOnly = "mirror-only"

// DeclaredGoProxyMirrorOnly selects the resolver chain for every leaf in this
// repository-authored Workflow. Omission preserves the default public fallback.
func DeclaredGoProxyMirrorOnly(workflow *wfv1.Workflow) (bool, error) {
	if workflow == nil {
		return false, nil
	}
	value, declared := workflow.Annotations[GoProxyAnnotation]
	if !declared {
		return false, nil
	}
	if value != GoProxyMirrorOnly {
		return false, fmt.Errorf("argoworkflow: %s must be %q when present; got %q", GoProxyAnnotation, GoProxyMirrorOnly, value)
	}
	return true, nil
}
