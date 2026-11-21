package stepmodel

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestRunSummaryFlattensCountsAndPreservesNulls(t *testing.T) {
	raw, err := json.Marshal(RunSummary{LogicalStepCounts: LogicalStepCounts{Total: 2, Exact: true, Pending: 2}})
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatal(err)
	}
	cases := []struct{ key, want string }{
		{"step_count", "2"}, {"steps_planned_exact", "true"}, {"pending", "2"},
		{"commit_sha", "null"}, {"started_at", "null"}, {"finished_at", "null"},
	}
	for _, tc := range cases {
		t.Run(tc.key, func(t *testing.T) {
			if string(fields[tc.key]) != tc.want {
				t.Errorf("got %s, want %s", fields[tc.key], tc.want)
			}
		})
	}
	for _, key := range []string{"LogicalStepCounts", "steps", "native_phase", "next_step_offset", "failure"} {
		if _, found := fields[key]; found {
			t.Errorf("unexpected key %q", key)
		}
	}
}

func TestExplicitEmptyDetailCollectionsAreArrays(t *testing.T) {
	cases := []struct {
		value  any
		fields []string
	}{
		{RunSummary{Steps: []RunStep{}}, []string{"steps"}},
		{RunStep{Attempts: []StepAttempt{}, Logs: []LogSelector{}}, []string{"attempts", "logs"}},
	}
	for _, tc := range cases {
		raw, err := json.Marshal(tc.value)
		if err != nil {
			t.Fatal(err)
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(raw, &fields); err != nil {
			t.Fatal(err)
		}
		for _, field := range tc.fields {
			if string(fields[field]) != "[]" {
				t.Fatalf("%s is %s", field, fields[field])
			}
		}
	}
}

func TestRunSummaryJSONRoundTripRetainsDetailPresence(t *testing.T) {
	for _, steps := range [][]RunStep{nil, {}, {{Path: "main", Status: StepPending, Attempts: []StepAttempt{}, Logs: []LogSelector{}}}} {
		original := RunSummary{Status: RunQueued, Steps: steps, LogicalStepCounts: LogicalStepCounts{Total: 3, Exact: true, Pending: 3}}
		raw, err := json.Marshal(original)
		if err != nil {
			t.Fatal(err)
		}
		var decoded RunSummary
		if err := json.Unmarshal(raw, &decoded); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(original, decoded) {
			t.Fatalf("round trip changed detail: %s", raw)
		}
	}
}

func TestEmbeddingRunValuesPreservesOuterFields(t *testing.T) {
	for _, value := range []any{
		struct {
			RunSummary
			Holds []string `json:"holds"`
		}{RunSummary: RunSummary{ID: "run", Steps: []RunStep{}}, Holds: []string{"manual"}},
		struct {
			RunStep
			Holds []string `json:"holds"`
		}{RunStep: RunStep{Path: "main", Attempts: []StepAttempt{}, Logs: []LogSelector{}}, Holds: []string{"manual"}},
	} {
		raw, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(raw, &fields); err != nil {
			t.Fatal(err)
		}
		if string(fields["holds"]) != `["manual"]` {
			t.Fatalf("embedding lost outer field: %s", raw)
		}
		if _, run := fields["id"]; run && string(fields["steps"]) != "[]" {
			t.Fatal("lost empty step page")
		}
		if _, step := fields["plan_path"]; step && (string(fields["attempts"]) != "[]" || string(fields["logs"]) != "[]") {
			t.Fatal("lost empty step detail")
		}
	}
}
