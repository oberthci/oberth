package argoworkflow

import (
	"strings"
	"testing"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
)

// Platform class selection must remain unreachable from repository-authored
// workflows, including resource pods and the VMIs that create launcher pods.
func TestPlatformPriorityClassesCannotBeSelectedByCI(t *testing.T) {
	mustAdmit(t, admissibleDocument, Policy{})
	refuse(t, strings.Replace(admissibleDocument, "  entrypoint: main\n", "  entrypoint: main\n  priority: 1000000\n", 1), Policy{}, "spec.priority")
	for _, component := range []string{"oberth", "argo-controller", "openbao"} {
		class := "oberth-oberth-platform-" + component
		t.Run(component, func(t *testing.T) {
			t.Run("workflow", func(t *testing.T) {
				doc := strings.Replace(admissibleDocument, "  entrypoint: main\n", "  entrypoint: main\n  podPriorityClassName: "+class+"\n", 1)
				refuse(t, doc, Policy{}, "spec.podPriorityClassName")
			})
			t.Run("template", func(t *testing.T) {
				doc := strings.Replace(admissibleDocument, "    - name: main\n", "    - name: main\n      priorityClassName: "+class+"\n", 1)
				refuse(t, doc, Policy{}, "priority overrides")
			})
			t.Run("pod-spec-patch", func(t *testing.T) {
				doc := strings.Replace(admissibleDocument, "  entrypoint: main\n", "  entrypoint: main\n  podSpecPatch: '{\"priorityClassName\":\""+class+"\"}'\n", 1)
				refuse(t, doc, Policy{}, "podSpecPatch")
			})
			t.Run("virtual-machine-launcher", func(t *testing.T) {
				manifest := strings.Replace(admittedVMIManifest, "spec:\n", "spec:\n  priorityClassName: "+class+"\n", 1)
				resource := &wfv1.ResourceTemplate{Action: "create", SetOwnerReference: true, Manifest: manifest}
				if err := admitVMIResource("resource", resource, []string{"quay.io/example/"}); err == nil || !strings.Contains(err.Error(), "priorityClassName") {
					t.Fatalf("VMI priority selection was not refused: %v", err)
				}
			})
		})
	}
}
