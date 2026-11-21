package stateport_test

import (
	"github.com/oberthci/oberth/internal/admissioncontract"
	"github.com/oberthci/oberth/internal/stateport"
	"github.com/oberthci/oberth/internal/stepmodel"
	"testing"
)

func TestArmedEvidenceUsesFinalNativeAttempt(t *testing.T) {
	plan := admissioncontract.Plan{Entries: []admissioncontract.PlanEntry{{Path: "build", HoldEligible: true, HoldArmed: true}}}
	for _, test := range []struct {
		name     string
		nodes    []stateport.Node
		excluded bool
	}{
		{"missing", nil, true},
		{"failed", []stateport.Node{{Path: "build", Status: stepmodel.StepFailed}}, true},
		{"unknown", []stateport.Node{{Path: "build", Status: stepmodel.StepUnknown}}, true},
		{"skipped", []stateport.Node{{Path: "build", Status: stepmodel.StepSkipped}}, true},
		{"never_run_skip", []stateport.Node{{Path: "build", Status: stepmodel.StepSkipped, NeverRunSkip: true}}, false},
		{"skip_with_pod", []stateport.Node{{Path: "build", Status: stepmodel.StepSkipped, NeverRunSkip: true, PodName: "executed"}}, true},
		{"skip_with_exit", []stateport.Node{{Path: "build", Status: stepmodel.StepSkipped, NeverRunSkip: true, Exit: new(0)}}, true},
		{"failed_skip_mark", []stateport.Node{{Path: "build", Status: stepmodel.StepFailed, NeverRunSkip: true}}, true},
		{"restarted_passed", []stateport.Node{{Path: "build", Status: stepmodel.StepPassed, PodRestarted: true}}, true},
		{"restarted_skipped", []stateport.Node{{Path: "build", Status: stepmodel.StepSkipped, NeverRunSkip: true, PodRestarted: true}}, true},
		{"passed", []stateport.Node{{Path: "build", Status: stepmodel.StepPassed}}, false},
		{"retry_passed", []stateport.Node{{Path: "build", Attempt: 2, Status: stepmodel.StepPassed}, {Path: "build", Attempt: 1, Status: stepmodel.StepFailed}}, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := stateport.ArmedStepMayHaveHeld(plan, test.nodes); got != test.excluded {
				t.Fatal("wrong evidence decision", got, test)
			}
		})
	}
	plan.Entries = append(plan.Entries, admissioncontract.PlanEntry{Path: "unarmed", HoldEligible: true})
	mixed := []stateport.Node{{Path: "build", Status: stepmodel.StepPassed}, {Path: "unarmed", Status: stepmodel.StepFailed}}
	if stateport.ArmedStepMayHaveHeld(plan, mixed) {
		t.Fatal("eligible unarmed tolerated failure excluded run")
	}
	mixed[1].Status, mixed[1].PodRestarted = stepmodel.StepPassed, true
	if stateport.ArmedStepMayHaveHeld(plan, mixed) {
		t.Fatal("unarmed Pod restart excluded run")
	}
	plan.Entries[0].HoldArmed = false
	if stateport.ArmedStepMayHaveHeld(plan, nil) {
		t.Fatal("eligibility excluded an unarmed run")
	}
}
func TestUnarmedDynamicEvidenceRemainsOrdinary(t *testing.T) {
	plan := admissioncontract.Plan{Entries: []admissioncontract.PlanEntry{{Path: "loop", Dynamic: true}}}
	nodes := []stateport.Node{{Path: "loop(0:a)", Status: stepmodel.StepPassed}, {Path: "loop(1:b)", Status: stepmodel.StepFailed}}
	if stateport.ArmedStepMayHaveHeld(plan, nodes) {
		t.Fatal("unarmed failed loop changed evidence eligibility")
	}
	nodes[1].Status = stepmodel.StepPassed
	if stateport.ArmedStepMayHaveHeld(plan, nodes) {
		t.Fatal("successful loop lost eligibility")
	}
}

func TestUnarmedParentRetryHistoryIsOrdinary(t *testing.T) {
	plan := admissioncontract.Plan{Entries: []admissioncontract.PlanEntry{{Path: "group.build"}}}
	nodes := []stateport.Node{{Path: "group.build", Attempt: 9, Order: []int{0, 9}, Status: stepmodel.StepFailed}, {Path: "group.build", Attempt: 1, Order: []int{1, 0}, Status: stepmodel.StepPassed}}
	if stateport.ArmedStepMayHaveHeld(plan, nodes) {
		t.Fatal("unarmed parent retry changed evidence eligibility")
	}
}
