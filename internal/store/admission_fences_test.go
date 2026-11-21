package store

import (
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/admissioncontract"
	"github.com/oberthci/oberth/internal/publicerr"
	"github.com/oberthci/oberth/internal/stateport"
	"github.com/oberthci/oberth/internal/stepmodel"
)

func sealView(t *testing.T, view admissioncontract.PreparedView) *admissioncontract.PreparedAdmission {
	t.Helper()
	prepared, err := admissioncontract.Seal(view)
	requireOK(t, err)
	return prepared
}

func TestAcceptCommitAndDuplicatePlanBinding(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"commit", "duplicate-path", "duplicate-root"} {
		t.Run(mode, func(t *testing.T) {
			s := testStore(t)
			_, input := accepted(t, s, "source")
			input = reacceptance(t, input, "bad")
			switch mode {
			case "commit":
				input.Request.CommitSHA = new(strings.Repeat("b", 40))
			case "duplicate-path", "duplicate-root":
				view := input.Prepared.View()
				if mode == "duplicate-root" {
					view.Plan.Entries[0].Path = ""
				}
				view.Plan.Entries = append(view.Plan.Entries, view.Plan.Entries[0])
				input.Prepared = sealView(t, view)
			}
			_, err := acceptForTest(t.Context(), s, input)
			requireCode(t, err, publicerr.InvalidInput)
			_, err = s.Run(t.Context(), "bad")
			requireCode(t, err, publicerr.NotFound)
		})
	}
}

func TestAcceptBatchPreservesUniqueRootLeafPlanAndBinds(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	_, input := accepted(t, s, "source")
	input = reacceptance(t, input, "root")
	view := input.Prepared.View()
	view.Plan.Entries[0].Path = ""
	input.Prepared = sealView(t, view)
	batch := stateport.BatchAcceptance{ReceiveID: input.Request.ReceiveID, Repository: input.Request.Repo, Actor: input.Request.Actor, Runs: []stateport.Acceptance{input}}
	recorded, err := s.AcceptBatch(t.Context(), batch)
	rows, fresh := recorded.Runs, recorded.Fresh
	requireOK(t, err)
	if !fresh || len(rows) != 1 {
		t.Fatal("root leaf was not accepted", rows, fresh)
	}
	stored, err := s.Run(t.Context(), rows[0].ID)
	requireOK(t, err)
	if encode(stored.Admission.Plan) != encode(view.Plan) || stored.Admission.Digest != input.Prepared.View().Digest {
		t.Fatal("stored root plan differs from sealed plan", stored.Admission.Plan)
	}
	requireOK(t, s.BindWorkflow(t.Context(), stateport.Action{Actor: input.Request.Actor.Identity}, runBinding(stored)))
	bound, err := s.Run(t.Context(), stored.ID)
	requireOK(t, err)
	if bound.Origin.WorkflowUID == "" {
		t.Fatal("root leaf was not bound")
	}
	evidence := evidenceInput{RunID: stored.ID, Nodes: []stateport.Node{{NodeID: "root-node", Path: "", Attempt: 1, Status: stepmodel.StepPassed, Credential: new(false), Exit: new(0)}}}
	requireOK(t, putNodes(t, s, evidence))
	requireOK(t, completeEvidence(t, s, evidenceInput{RunID: stored.ID}))
	observed, err := s.Run(t.Context(), stored.ID)
	requireOK(t, err)
	if !observed.EvidenceComplete || observed.Passed != 0 || observed.Pending != 1 {
		t.Fatal("admitted root evidence was not complete", observed)
	}
	requireOK(t, s.Finish(t.Context(), stateport.Action{}, stateport.RunUpdate{ID: stored.ID, Revision: observed.Revision, Status: stepmodel.RunPassed}))
}
