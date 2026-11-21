package api_test

import (
	"encoding/json"
	"testing"

	"github.com/oberthci/oberth/internal/api"
	"github.com/oberthci/oberth/internal/api/fake"
	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/stepmodel"
)

// TestRunRecordCarriesCauseAsPlainFields pins the cause beside the run facts
// in the dashboard read and in the declared run_get and run_list results.
func TestRunRecordCarriesCauseAsPlainFields(t *testing.T) {
	run := apicontract.RunRecord{RunSummary: stepmodel.RunSummary{ID: "12345678", Repo: "origin/team/widget", Ref: "main", Status: stepmodel.RunPassed},
		RunCause: model.RunCause{Actor: "oberth:scheduler", Trigger: model.TriggerSchedule, ScheduleEntry: "nightly", AssistRequested: true}}
	f := setup(t, func(_ *api.Config, b *apicontract.Bindings) {
		b.RunGet.Handler = &fake.Handler[apicontract.RunGetArgs, apicontract.RunRecord]{Value: run}
	})
	w := request(f, "GET", "/api/runs/12345678?view=dashboard", nil)
	assertSuccess(t, w)
	var shown map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &shown); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"actor": "oberth:scheduler", "trigger": "schedule", "schedule_entry": "nightly", "assist_requested": true, "rerun_of": ""}
	for key, value := range want {
		if shown[key] != value {
			t.Fatalf("%s = %v, want %v", key, shown[key], value)
		}
	}
	r, err := api.Builtin(fake.Bindings())
	if err != nil {
		t.Fatal(err)
	}
	declared := 0
	for _, tool := range listTools(t, r, admin()) {
		var properties apicontract.Schema
		switch tool.Name {
		case "run_get":
			properties = tool.OutputSchema["properties"].(apicontract.Schema)
		case "run_list":
			runs := tool.OutputSchema["properties"].(apicontract.Schema)["runs"].(apicontract.Schema)
			properties = runs["items"].(apicontract.Schema)["properties"].(apicontract.Schema)
		default:
			continue
		}
		for key := range want {
			if properties[key] == nil {
				t.Fatalf("%s result does not declare %s", tool.Name, key)
			}
		}
		declared++
	}
	if declared != 2 {
		t.Fatalf("checked %d of 2 run results", declared)
	}
}
