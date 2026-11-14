package argojob

import (
	"reflect"
	"testing"

	corev1 "k8s.io/api/core/v1"
)

func TestControllerExecutorScratchVolumeIsPreserved(t *testing.T) {
	for _, script := range []bool{false, true} {
		shape := "container"
		if script {
			shape = "script"
		}
		before := nonrootControllerFixture(t, shape+"-before-patch.json")
		wf := authoredNonrootFixture(t, script)
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
