package argojob

import (
	"encoding/json"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

func TestNativeCommandsAndPlacementReceiveNoMeasurementInstrumentation(t *testing.T) {
	for _, selector := range []map[string]string{nil, {"kubernetes.io/arch": "author-architecture"}} {
		t.Run(map[bool]string{true: "none", false: "authored"}[selector == nil], func(t *testing.T) {
			wf := nativeWorkflow(t)
			wf.Spec.NodeSelector = selector
			leaf := &wf.Spec.Templates[0]
			leaf.NodeSelector = selector
			leaf.Container.Command = []string{"/bin/sh", "-ec"}
			leaf.Container.Args = []string{"printf '%s' unchanged"}
			leaf.Container.Resources.Limits = corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("512Mi")}
			before := leaf.Container.DeepCopy()
			posted, seeds := nativeSubmitWithSeedCalls(t, wf)
			nativeEqual(t, wf.Spec.NodeSelector, posted.Spec.NodeSelector)
			nativeEqual(t, nativePatchPlacement(t, wf.Spec.PodSpecPatch), nativePatchPlacement(t, posted.Spec.PodSpecPatch))
			for _, template := range posted.Spec.Templates {
				nativeEqual(t, leaf.NodeSelector, template.NodeSelector)
				nativeEqual(t, nativePatchPlacement(t, leaf.PodSpecPatch), nativePatchPlacement(t, template.PodSpecPatch))
				if template.Container != nil {
					nativeEqual(t, before.Command, template.Container.Command)
					nativeEqual(t, before.Args, template.Container.Args)
					nativeEqual(t, before.Resources, template.Container.Resources)
				}
			}
			raw, err := json.Marshal(posted)
			if err != nil {
				t.Fatal(err)
			}
			for _, forbidden := range []string{"/run/oberth/usage", "internal-step-exec", "step-usage-memory"} {
				if strings.Contains(string(raw), forbidden) {
					t.Fatal("measurement remains in submitted Workflow", forbidden)
				}
			}
			if len(seeds) != 1 {
				t.Fatalf("ordinary run copied an executable: %d seed execs", len(seeds))
			}
		})
	}
}
