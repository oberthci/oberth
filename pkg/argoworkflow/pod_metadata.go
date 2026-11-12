package argoworkflow

import (
	"fmt"
	"strings"

	k8svalidation "k8s.io/apimachinery/pkg/util/validation"
)

// These exact keys are used by the Oberth chart, managed dependency charts or
// installer selectors. Reserve keys, not values: Argo can interpolate values.
// Keep the rendered-selector regression in internal/installer in sync when
// changing a chart or pin. Empty selectors already cover every Pod in scope.
func reservedPodMetadataKey(key string) bool {
	if strings.HasPrefix(key, "oberth.ci/") || strings.HasPrefix(key, "workflows.argoproj.io/") {
		return true
	}
	switch key {
	case "app.kubernetes.io/name", "app.kubernetes.io/instance", "app.kubernetes.io/component",
		"component", "openbao-active", "openbao-internal", "kubernetes.io/metadata.name",
		"kubectl.kubernetes.io/default-container":
		return true
	default:
		return false
	}
}

func admitPodMetadataKeys(location string, entries map[string]string) []error {
	var problems []error
	for key := range entries {
		if len(k8svalidation.IsQualifiedName(key)) != 0 {
			// Computed keys could become reserved selectors after Argo substitution.
			problems = append(problems, fmt.Errorf("argoworkflow: %s[%q] must be a literal Kubernetes qualified name", location, key))
		} else if reservedPodMetadataKey(key) {
			problems = append(problems, fmt.Errorf("argoworkflow: %s[%q] is reserved for Oberth, Argo or installed selectors", location, key))
		}
	}
	return problems
}
