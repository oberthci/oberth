package argojob

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/oberthci/oberth/pkg/argoworkflow"
	"github.com/oberthci/oberth/pkg/pipeline"
)

func TestBuildPreservesAuthoredWorkflowCleanup(t *testing.T) {
	for _, authored := range []string{"", `  ttlStrategy:
    secondsAfterCompletion: 1
    secondsAfterSuccess: 2
    secondsAfterFailure: 3
  podGC:
    strategy: OnPodCompletion
    deleteDelayDuration: 2s
    labelSelector:
      matchLabels: {example.com/cleanup: yes}
`} {
		t.Run(authored, func(t *testing.T) {
			source := strings.Replace(greedyDocument, "spec:\n", "spec:\n"+authored, 1)
			before, err := argoworkflow.Decode([]byte(source))
			if err != nil {
				t.Fatal(err)
			}
			after, err := Build(testConfig(), testRequest(pipeline.TriggerCI, source))
			if err != nil {
				t.Fatal(err)
			}
			for name, pair := range map[string][2]any{"ttlStrategy": {before.Spec.TTLStrategy, after.Spec.TTLStrategy}, "podGC": {before.Spec.PodGC, after.Spec.PodGC}} {
				want, _ := json.Marshal(pair[0])
				got, _ := json.Marshal(pair[1])
				if string(got) != string(want) {
					t.Fatalf("%s changed: got %s, want %s", name, got, want)
				}
			}
		})
	}
}
