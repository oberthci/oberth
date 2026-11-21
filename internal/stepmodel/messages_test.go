package stepmodel

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/oberthci/oberth/internal/stepmodel/steptest"
)

func TestRunAndStepKeepShortReasonsAndFullDetails(t *testing.T) {
	const detail = "Container startup reported a detailed native message.\nPod: step-1; exit code: 17."
	run := RunSummary{DisplayReason: "image pull failed", Detail: detail,
		Steps: []RunStep{{Path: "main", DisplayReason: "waiting for a node", Detail: detail}}}
	steptest.AssertReasons(t, run.DisplayReason, run.Steps[0].DisplayReason)
	raw, err := json.Marshal(run)
	if err != nil {
		t.Fatal(err)
	}
	var decoded RunSummary
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(run, decoded) {
		t.Fatalf("wire changed reason/detail: %s", raw)
	}
	if !reflect.DeepEqual(run, run.Clone()) {
		t.Fatal("clone changed reason/detail")
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatal(err)
	}
	if string(fields["display_reason"]) != `"image pull failed"` {
		t.Fatal("missing short reason")
	}
	var full string
	if err := json.Unmarshal(fields["detail"], &full); err != nil {
		t.Fatal(err)
	}
	if full != detail {
		t.Fatal("full detail was truncated")
	}
}

func TestEmptyDetailsAreOmitted(t *testing.T) {
	for _, value := range []any{RunSummary{}, RunStep{}} {
		raw, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(raw, &fields); err != nil {
			t.Fatal(err)
		}
		if _, ok := fields["detail"]; ok {
			t.Fatal("empty detail emitted", string(raw))
		}
	}
}
