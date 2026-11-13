package argojob

import (
	"reflect"
	"testing"

	corev1 "k8s.io/api/core/v1"
)

func TestControllerExecutorScratchVolumeIsPreserved(t *testing.T) {
	for _, size := range []string{"", "4Gi", "1024Mi", "1.5Gi"} {
		for _, script := range []bool{false, true} {
			shape := "container"
			if script {
				shape = "script"
			}
			before := nonrootControllerFixture(t, shape+"-before-patch.json")
			wf := authoredNonrootWithScratch(t, script, size)
			after := nonrootApplyControllerFixture(t, before, wf)
			find := func(pod *corev1.Pod) *corev1.Volume {
				for i := range pod.Spec.Volumes {
					if pod.Spec.Volumes[i].Name == "var-run-argo" {
						return &pod.Spec.Volumes[i]
					}
				}
				return nil
			}
			if find(before) == nil || !reflect.DeepEqual(find(before), find(after)) {
				t.Fatalf("native executor scratch was rewritten: before=%+v after=%+v", find(before), find(after))
			}
		}
	}
}

func TestExecutorScratchRejectsInvalidLimits(t *testing.T) {
	for _, size := range []string{"invalid", "0", "-1Gi", "2Pi", " "} {
		config := testConfig()
		config.ExecutorScratchSizeLimit = size
		if err := config.Validate(); err == nil {
			t.Fatalf("accepted invalid executor scratch limit %q", size)
		}
	}
}
