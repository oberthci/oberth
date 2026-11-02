package model

import (
	"encoding/json"
	"testing"
)

func TestArgoNamesPreserveRetainedJSONFields(t *testing.T) {
	var run Run
	if err := json.Unmarshal([]byte(`{"JobName":"wf","FailedBurn":"ci"}`), &run); err != nil {
		t.Fatal(err)
	}
	if run.WorkflowName != "wf" || run.FailedStage != "ci" {
		t.Fatalf("old run evidence no longer decodes: %+v", run)
	}
	var step StepResult
	if err := json.Unmarshal([]byte(`{"Burn":"ci","Step":"unit"}`), &step); err != nil {
		t.Fatal(err)
	}
	if step.Stage != "ci" || step.Step != "unit" {
		t.Fatalf("old step evidence no longer decodes: %+v", step)
	}
	for _, value := range []any{run, step} {
		data, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(data, &fields); err != nil {
			t.Fatal(err)
		}
		for _, name := range []string{"WorkflowName", "FailedStage", "Stage"} {
			if _, ok := fields[name]; ok {
				t.Errorf("internal name %s leaked into retained JSON: %s", name, data)
			}
		}
	}
}
