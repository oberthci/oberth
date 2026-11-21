package stateport

import (
	"github.com/oberthci/oberth/internal/admissioncontract"
	"github.com/oberthci/oberth/internal/stepmodel"
	"slices"
	"testing"
)

func TestBExpandedStepsRetainNumericOrderAndCredentialIntent(t *testing.T) {
	plan := admissioncontract.Plan{Entries: []admissioncontract.PlanEntry{{Path: "before"}, {Path: "fan", Dynamic: true, CredentialIntent: true}, {Path: "after"}}}
	rows := []stepmodel.RunStep{{Path: "after"}, {Path: "fan(10:item)"}, {Path: "before"}, {Path: "fan(2:item)"}, {Path: "fan(1:item)"}}
	result := PlanSteps(plan, rows)
	paths := []string{}
	for _, s := range result {
		paths = append(paths, s.Path)
	}
	if !slices.Equal(paths, []string{"before", "fan(1:item)", "fan(2:item)", "fan(10:item)", "after"}) {
		t.Fatal(paths)
	}
	for _, s := range result[1:4] {
		if !s.CredentialIntent {
			t.Fatal("expanded child lost plan intent", s)
		}
	}
}
