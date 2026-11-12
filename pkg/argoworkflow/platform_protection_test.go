package argoworkflow

import (
	"strings"
	"testing"
)

// Platform class selection must remain unreachable from repository-authored
// workflows. Resource capability tests cover embedded resource Pod templates.
func TestPlatformPriorityClassesCannotBeSelectedByCI(t *testing.T) {
	mustAdmit(t, admissibleDocument, Policy{})
	mustAdmit(t, strings.Replace(admissibleDocument, "  entrypoint: main\n", "  entrypoint: main\n  priority: 1000000\n", 1), Policy{})
	for _, component := range []string{"control-plane", "pipeline"} {
		class := "oberth-oberth-" + component
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
		})
	}
}
