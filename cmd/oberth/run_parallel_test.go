package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/api/fake"
	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/stepmodel"
)

func TestRunGetAndAgentBlockMarkParallelSteps(t *testing.T) {
	run := apicontract.RunRecord{RunSummary: stepmodel.RunSummary{ID: "run1", Status: stepmodel.RunFailed, Steps: []stepmodel.RunStep{
		{Path: "profiles", Name: "profiles", Status: stepmodel.StepPassed},
		{Path: "release-index", Name: "release-index", Status: stepmodel.StepFailed, ParallelGroup: 1},
		{Path: "tools-dir", Name: "tools-dir", Status: stepmodel.StepPassed, ParallelGroup: 1},
		{Path: "deprecations", Name: "deprecations", Status: stepmodel.StepPending, ParallelGroup: 1},
		{Path: "finish", Name: "finish", Status: stepmodel.StepPending},
	}}}
	b := fake.Bindings()
	b.RunGet.Handler = &fake.Handler[apicontract.RunGetArgs, apicontract.RunRecord]{Value: run}
	a, out, diag := fixture(t, b)
	if a.run(t.Context(), []string{"run", "get", "run1"}) != 0 {
		t.Fatal(diag.String())
	}
	raw, err := json.Marshal(run)
	if err != nil {
		t.Fatal(err)
	}
	block, ok := Display("run_get", raw)
	if !ok || block != out.String() {
		t.Fatal("agent display differs from run get", block, out.String())
	}
	for _, text := range []string{out.String(), block} {
		if strings.Count(text, "│ ") != 3 || !strings.Contains(text, "│ tools-dir") || !strings.Contains(text, "│ release-index") || !strings.Contains(text, "│ deprecations") || strings.Contains(text, "│ profiles") || strings.Contains(text, "│ finish") {
			t.Fatal("parallel step markers lost", text)
		}
	}
}
