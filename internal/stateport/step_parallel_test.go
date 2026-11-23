package stateport

import (
	"testing"

	"github.com/oberthci/oberth/internal/admissioncontract"
	"github.com/oberthci/oberth/internal/stepmodel"
)

func TestFanOutChildrenKeepAdmittedGroup(t *testing.T) {
	for _, fanOut := range []bool{false, true} {
		plan := admissioncontract.Plan{Entries: []admissioncontract.PlanEntry{{Path: "fan", Name: "fan", Dynamic: true, FanOut: fanOut, ParallelGroup: 7}}}
		r := Run{Admission: admissioncontract.PreparedView{Plan: plan}}
		steps := ProjectSteps(r, []Node{{Path: "fan(0:a)", NodeID: "a", Status: stepmodel.StepRunning}, {Path: "fan(1:b)", NodeID: "b", Status: stepmodel.StepRunning}})
		for i := range steps {
			steps[i].ParallelGroup = 99
		}
		steps = PlanSteps(plan, steps)
		want := 0
		if fanOut {
			want = 7
		}
		if len(steps) != 3 || steps[0].ParallelGroup != 7 || steps[1].ParallelGroup != want || steps[2].ParallelGroup != want {
			t.Fatal("runtime children lost their admitted grouping", fanOut, steps)
		}
	}
}
