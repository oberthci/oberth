package argoworkflow

import (
	"strings"
	"testing"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestTrustedTestsEntrypoint(t *testing.T) {
	for _, name := range []string{"producers", "", " producers", "producers ", "{{workflow.parameters.producer}}", "missing", "oberth-trusted-tests"} {
		t.Run(name, func(t *testing.T) {
			workflow := &wfv1.Workflow{ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{TrustedTestsEntrypointAnnotation: name}}, Spec: wfv1.WorkflowSpec{Entrypoint: "main", Templates: []wfv1.Template{{Name: "main"}, {Name: "producers"}, {Name: "oberth-trusted-tests"}}}}
			selected, err := TrustedTestsEntrypoint(workflow)
			if name == "producers" {
				if err != nil || selected != name || workflow.Spec.Entrypoint != "main" {
					t.Fatalf("selection mutated normal entrypoint: %q %v", selected, err)
				}
			} else {
				if err == nil || !strings.Contains(err.Error(), TrustedTestsEntrypointAnnotation) {
					t.Fatalf("accepted invalid selection %q: %v", name, err)
				}
				if err := Admit(workflow, Policy{}); err == nil || !strings.Contains(err.Error(), TrustedTestsEntrypointAnnotation) {
					t.Fatalf("admission missed invalid selection %q: %v", name, err)
				}
			}
			delete(workflow.Annotations, TrustedTestsEntrypointAnnotation)
			if selected, err := TrustedTestsEntrypoint(workflow); err != nil || selected != "main" {
				t.Fatalf("legacy entrypoint: %q %v", selected, err)
			}
		})
	}
}
