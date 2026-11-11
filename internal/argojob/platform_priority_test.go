package argojob

import (
	"testing"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/yaml"

	"github.com/oberthci/oberth/internal/podpriority"
	"github.com/oberthci/oberth/pkg/pipeline"
)

// Pipeline Pods use the non-preempting lower tier. The only
// Workflow queue priority is the trigger-based ReleasePriority on release
// runs (Argo's postponed queue and lock grants, never Pod scheduling).
func assertOrdinaryWorkflowPriority(t *testing.T, workflow *wfv1.Workflow) {
	t.Helper()
	release := workflow.Spec.PodMetadata != nil && workflow.Spec.PodMetadata.Labels[triggerLabel] == string(pipeline.TriggerRelease)
	if workflow.Spec.PodPriorityClassName != podpriority.PipelineClass(workflow.Namespace) || (workflow.Spec.Priority != nil && (!release || *workflow.Spec.Priority != ReleasePriority)) {
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
	var base corev1.PodSpec
	if err := yaml.Unmarshal([]byte(workflow.Spec.PodSpecPatch), &base); err != nil || base.PreemptionPolicy == nil || *base.PreemptionPolicy != corev1.PreemptNever {
		t.Fatal("pipeline Pods can preempt")
	}
	for _, template := range workflow.Spec.Templates {
		if template.PriorityClassName != "" {
			t.Fatalf("CI template %s received a priority override", template.Name)
		}
		assertPatch(template.PodSpecPatch)
	}
}

func assertPipelinePodPriority(t *testing.T, pod *corev1.Pod) {
	t.Helper()
	if pod.Spec.PriorityClassName != podpriority.PipelineClass(pod.Namespace) || pod.Spec.PreemptionPolicy == nil || *pod.Spec.PreemptionPolicy != corev1.PreemptNever || (pod.Spec.Priority != nil && *pod.Spec.Priority != podpriority.PipelineValue) {
		t.Fatalf("pipeline Pod received a preempting or control-plane priority: %s", pod.Name)
	}
}
