package store

import (
	"testing"

	"github.com/oberthci/oberth/internal/publicerr"
	"github.com/oberthci/oberth/internal/stateport"
	"github.com/oberthci/oberth/internal/stepmodel"
)

func TestFinishRejectsStaleAndTerminalRevisions(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	r, _ := accepted(t, s, "revision")
	action := stateport.Action{Actor: "operator"}
	stale := stateport.RunUpdate{ID: r.ID, Revision: r.Revision + 1, Status: stepmodel.RunPassed}
	requireCode(t, s.Finish(t.Context(), action, stale), publicerr.Conflict)
	current, err := s.Run(t.Context(), r.ID)
	requireOK(t, err)
	if current.Status != r.Status {
		t.Fatal("stale Finish changed verdict")
	}
	stale.Revision = r.Revision
	requireOK(t, s.Finish(t.Context(), action, stale))
	current, err = s.Run(t.Context(), r.ID)
	requireOK(t, err)
	stale.Revision = current.Revision
	stale.Status = stepmodel.RunFailed
	requireCode(t, s.Finish(t.Context(), action, stale), publicerr.Conflict)
	requireCode(t, s.BindWorkflow(t.Context(), action, runBinding(current)), publicerr.Conflict)
}

func TestRerunRecordingKeepsLineageWithoutRecheckingGatePolicy(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"restart", "passed", "newer", "different-ref", "release-incomplete"} {
		t.Run(mode, func(t *testing.T) {
			s := testStore(t)
			r, input := accepted(t, s, "source")
			status := stepmodel.RunFailed
			if mode == "passed" {
				status = stepmodel.RunPassed
			}
			requireOK(t, s.Finish(t.Context(), stateport.Action{Actor: "operator"}, stateport.RunUpdate{ID: r.ID, Revision: r.Revision, Status: status, Restart: true}))
			rerun := reacceptance(t, input, "rerun")
			rerun.RerunOf = r.ID
			changeRerunFixture(t, s, &rerun, mode)
			next, err := acceptForTest(t.Context(), s, rerun)
			if mode == "different-ref" {
				requireCode(t, err, publicerr.Conflict)
				return
			}
			requireOK(t, err)
			if next.Origin.RerunOf != r.ID {
				t.Fatal("restart rerun lost its source")
			}
		})
	}
}

func changeRerunFixture(t *testing.T, s *Store, input *stateport.Acceptance, mode string) {
	t.Helper()
	switch mode {
	case "newer":
		newer := reacceptance(t, *input, "newer")
		newer.RerunOf = ""
		_, err := acceptForTest(t.Context(), s, newer)
		requireOK(t, err)
	case "different-ref":
		input.Request.Ref = "refs/heads/other"
		view := input.Prepared.View()
		view.Ref = input.Request.Ref
		input.Prepared = sealView(t, view)
	case "release-incomplete":
		execute(t, s.writer, `UPDATE runs SET release=1 WHERE id='source'`)
	}
}

func TestPurgeRepeatsProtectionChecks(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	r, _ := accepted(t, s, "run")
	a := stateport.Action{Actor: "operator"}
	requireOK(t, s.Finish(t.Context(), a, stateport.RunUpdate{ID: r.ID, Revision: r.Revision, Status: stepmodel.RunPassed}))
	requireOK(t, s.BeginPurge(t.Context(), a, r.ID, "receipt"))
	requireOK(t, s.PreparePublication(t.Context(), a, publicationIntent(r)))
	requireCode(t, s.BeginPurge(t.Context(), a, r.ID, "receipt"), publicerr.Conflict)
	requireCode(t, s.FinishPurge(t.Context(), a, r.ID, "receipt"), publicerr.Conflict)
	receipt, err := s.PurgeReceipt(t.Context(), r.ID)
	requireOK(t, err)
	if receipt.Completed {
		t.Fatal("protected evidence purge acknowledged")
	}
}
func TestCancellationObligationRemainsReadable(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	r, _ := accepted(t, s, "cancel-reader")
	a := stateport.Action{Actor: "operator"}
	_, err := s.Cancel(t.Context(), a, stateport.RunCancellation{ID: r.ID, Revision: r.Revision})
	requireOK(t, err)
	rows, err := s.Obligations(t.Context(), r.ID)
	requireOK(t, err)
	if len(rows) != 1 || rows[0].Kind != "stop" || rows[0].Value != stateport.ObligationStop {
		t.Fatal(rows)
	}
	r, err = s.Run(t.Context(), r.ID)
	requireOK(t, err)
	requireOK(t, s.Finish(t.Context(), a, stateport.RunUpdate{ID: r.ID, Revision: r.Revision, Status: stepmodel.RunCancelled}))
	requireCode(t, s.BeginPurge(t.Context(), a, r.ID, "receipt"), publicerr.Conflict)
}

func TestAssistedWorkNeverSuppliesGreenEvidence(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	r, _ := acceptedMode(t, s, "assisted", true)
	a := stateport.Action{Actor: "operator"}
	err := s.Assist(t.Context(), a, stateport.HoldAction{RunID: r.ID, Step: "build", NodeID: "node", PodUID: "pod", Kind: "exec", Revision: r.Revision, Argv: []string{"true"}, TimeoutS: 60})
	requireOK(t, err)
	r, err = s.Run(t.Context(), r.ID)
	requireOK(t, err)
	requireOK(t, s.Finish(t.Context(), a, stateport.RunUpdate{ID: r.ID, Revision: r.Revision, Status: stepmodel.RunPassed}))
	requireCode(t, s.ProjectCI(t.Context(), a, r.ID), publicerr.Conflict)
	requireCode(t, s.PreparePublication(t.Context(), a, publicationIntent(r)), publicerr.Forbidden)
}
