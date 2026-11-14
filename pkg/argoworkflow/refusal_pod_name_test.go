package argoworkflow

import (
	"testing"

	k8svalidation "k8s.io/apimachinery/pkg/util/validation"
)

// Argo v4.0.8 workflow/util/pod_name.go:50-71 preserves the template name
// in v2 descendant names and appends the FNV-1a hash of the node name.
// The entrypoint uses the workflow name unchanged. Keep these known outputs
// here so this naming proof does not import controller-only dependencies.
func TestRefusalUppercaseLeafPodName(t *testing.T) {
	for _, tc := range []struct {
		name  string
		pod   string
		valid bool
	}{
		{"uppercase descendant", "workflow-camelCase-662066643", false},
		{"lowercase descendant", "workflow-lowercase-662066643", true},
		{"entrypoint", "workflow", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			problems := k8svalidation.IsDNS1123Subdomain(tc.pod)
			if (len(problems) == 0) != tc.valid {
				t.Fatalf("Pod name %q validation = %v, want valid=%v", tc.pod, problems, tc.valid)
			}
		})
	}
}
