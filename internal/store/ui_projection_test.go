package store

import (
	"github.com/oberthci/oberth/internal/admissioncontract"
	"github.com/oberthci/oberth/internal/stateport"
	"github.com/oberthci/oberth/internal/stepmodel"
	"testing"
)

func TestStepCredentialIntentFromPlan(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	_, input := accepted(t, s, "credential-source")
	view := input.Prepared.View()
	view.RunID = "credential-projection"
	view.Plan.Entries = []admissioncontract.PlanEntry{{Path: "build", Name: "build", CredentialIntent: true}, {Path: "test", Name: "test"}}
	var err error
	input.Prepared, err = admissioncontract.Seal(view)
	requireOK(t, err)
	input.Request.ReceiveID = "credential-receive"
	run, err := acceptForTest(t.Context(), s, input)
	requireOK(t, err)
	steps, err := s.Steps(t.Context(), run.ID)
	requireOK(t, err)
	if len(steps) != 2 || !steps[0].CredentialIntent || steps[1].CredentialIntent {
		t.Fatal("credential intent did not come from each plan entry", steps)
	}
}

func TestStepProjectionPreservesPlanOrderAndFoldsRetries(t *testing.T) {
	t.Parallel()
	plan := admissioncontract.Plan{Exact: true, Entries: []admissioncontract.PlanEntry{{Path: "z-build", Name: "build"}, {Path: "a-test", Name: "test"}}}
	got := stateport.ProjectSteps(stateport.Run{Admission: admissioncontract.PreparedView{Plan: plan}}, []stateport.Node{
		{NodeID: "attempt-two", Path: "z-build", Attempt: 2, Status: stepmodel.StepFailed},
		{NodeID: "attempt-one", Path: "z-build", Attempt: 1, Status: stepmodel.StepFailed},
	})
	if len(got) != 2 || got[0].Path != "z-build" || got[1].Path != "a-test" || got[0].AttemptCount != 2 {
		t.Fatal("plan order or retry folding changed", got)
	}
}
