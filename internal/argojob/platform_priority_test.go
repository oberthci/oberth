package argojob

import (
	"testing"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/yaml"

	"github.com/oberthci/oberth/pkg/pipeline"
)

// assertOrdinaryWorkflowPriority: no run gets a Pod priority. The only
// Workflow queue priority is the trigger-based ReleasePriority on release
// runs (Argo's postponed queue and lock grants, never Pod scheduling).
func assertOrdinaryWorkflowPriority(t *testing.T, workflow *wfv1.Workflow) {
	t.Helper()
	release := workflow.Spec.PodMetadata != nil && workflow.Spec.PodMetadata.Labels[triggerLabel] == string(pipeline.TriggerRelease)
	if workflow.Spec.PodPriorityClassName != "" || (workflow.Spec.Priority != nil && (!release || *workflow.Spec.Priority != ReleasePriority)) {
		t.Fatal("CI workflow received a priority override")
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
