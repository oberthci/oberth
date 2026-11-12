package argojob

import (
	"testing"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/yaml"
)

// Ordinary source without a priority class receives no server priority override.
func assertOrdinaryWorkflowPriority(t *testing.T, workflow *wfv1.Workflow) {
	t.Helper()
	if workflow.Spec.PodPriorityClassName != "" {
		t.Fatal("user Workflow has an injected Pod priority class")
	}
	assertPatch := func(patch string) {
		t.Helper()
		var pod corev1.PodSpec
		if err := yaml.Unmarshal([]byte(patch), &pod); err != nil {
			t.Fatal(err)
		}
		if pod.PriorityClassName != "" || pod.Priority != nil {
			t.Fatal("CI pod patch received a priority override")
		}
	}
	assertPatch(workflow.Spec.PodSpecPatch)
	for _, template := range workflow.Spec.Templates {
		if template.PriorityClassName != "" {
			t.Fatalf("CI template %s received a priority override", template.Name)
		}
		assertPatch(template.PodSpecPatch)
	}
}
