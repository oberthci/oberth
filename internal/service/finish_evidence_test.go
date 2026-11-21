package service_test

import (
	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/publicerr"
	"github.com/oberthci/oberth/internal/service"
	"github.com/oberthci/oberth/internal/stateport"
	"github.com/oberthci/oberth/internal/stepmodel"
	"testing"
)

func TestArmedFailureFinishesWithoutPublication(t *testing.T) {
	t.Parallel()
	for _, missing := range []bool{false, true} {
		t.Run(map[bool]string{false: "failed", true: "missing"}[missing], func(t *testing.T) {
			f := setup(t)
			r := f.seed(stepmodel.RunRunning)
			r.Admission.Plan.Entries[0].HoldArmed = true
			f.store.native.SeedRun(r)
			if !missing {
				f.store.native.SeedNodes(r.ID, []stateport.Node{{NodeID: "native", Path: "build", Attempt: 1, Status: stepmodel.StepFailed, Exit: new(7)}})
			}
			if err := f.s.Finished(t.Context(), service.FinishedRun{RunID: r.ID, Status: stepmodel.RunPassed, NativePhase: new(stepmodel.NativeSucceeded)}); err != nil {
				t.Fatal(err)
			}
			stored, err := f.store.Run(t.Context(), r.ID)
			if err != nil || stored.Status != stepmodel.RunPassed || !stored.Origin.Assisted || len(f.store.native.PublicationValues()) != 0 {
				t.Fatal("held failure changed native result or supplied evidence", stored, err)
			}
			_, err = f.s.PreparePromote(t.Context(), workerCaller(), apicontract.PromoteArgs{Repo: repository, SHA: head})
			requireCode(t, err, publicerr.Forbidden)
			page, err := f.s.EventsSince(t.Context(), caller(), eventsInput{})
			if err != nil || len(page.Events) != 1 || page.Events[0].Kind != "run.finished" {
				t.Fatal("finish event missing", page, err)
			}
		})
	}
}
