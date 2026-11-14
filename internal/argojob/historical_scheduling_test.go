package argojob

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
)

func assertHistoricalWorkflowPriority(t *testing.T, pod *corev1.Pod) {
	t.Helper()
	if pod.Spec.PriorityClassName != "oberth-argo-oberth-pipeline" || pod.Spec.PreemptionPolicy == nil || *pod.Spec.PreemptionPolicy != corev1.PreemptNever {
		t.Fatal("historical Workflow Pod lost its captured non-preempting class")
	}
}
