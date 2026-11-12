package argojob

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/oberthci/oberth/pkg/argoworkflow"
	"github.com/oberthci/oberth/pkg/pipeline"
)

func TestAuthoredPriorityAndSynchronizationSurviveEveryRunKind(t *testing.T) {
	for _, trigger := range []pipeline.Trigger{pipeline.TriggerCI, pipeline.TriggerRelease} {
		source := strings.Replace(greedyDocument, "  entrypoint: main", "  priority: 87\n  synchronization:\n    mutexes:\n    - name: oberth-release\n  entrypoint: main", 1)
		original, err := argoworkflow.Decode([]byte(source))
		if err != nil {
			t.Fatal(err)
		}
		actual, err := Build(testConfig(), testRequest(trigger, source))
		if err != nil {
			t.Fatal(err)
		}
		before, _ := json.Marshal(original.Spec.Synchronization)
		after, _ := json.Marshal(actual.Spec.Synchronization)
		if actual.Spec.Priority == nil || *actual.Spec.Priority != 87 || string(before) != string(after) {
			t.Fatalf("%s: authored scheduling changed: %s -> %s, priority %v", trigger, before, after, actual.Spec.Priority)
		}
	}
}
