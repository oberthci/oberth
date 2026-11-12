package argojob

import (
	"encoding/json"
	"testing"

	"github.com/oberthci/oberth/pkg/argoworkflow"
	"github.com/oberthci/oberth/pkg/pipeline"
)

func TestNativeSubmissionLeavesUndeclaredResourcesAndParallelismAlone(t *testing.T) {
	request := testRequest(pipeline.TriggerCI, greedyDocument)
	original, err := argoworkflow.Decode(request.Source)
	if err != nil {
		t.Fatal(err)
	}
	wf, err := Build(testConfig(), request)
	if err != nil {
		t.Fatal(err)
	}
	if wf.Spec.Parallelism != nil {
		t.Fatal("injected workflow parallelism", wf.Spec.Parallelism)
	}
	for _, template := range wf.Spec.Templates {
		if template.Name != "main" || template.Container == nil {
			continue
		}
		before, _ := json.Marshal(original.Spec.Templates[0].Container.Resources)
		after, _ := json.Marshal(template.Container.Resources)
		if string(before) != string(after) {
			t.Fatalf("resources changed: %s -> %s", before, after)
		}
		for _, key := range []string{"oberth.ci/cpu-requests", "oberth.ci/request-defaults"} {
			if _, ok := wf.Annotations[key]; ok {
				t.Fatal("retired request policy annotation", key)
			}
		}
		return
	}
	t.Fatal("main template missing")
}
