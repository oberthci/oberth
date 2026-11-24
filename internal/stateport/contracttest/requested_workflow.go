package contracttest

import (
	"testing"

	"github.com/oberthci/oberth/internal/admissioncontract"
	"github.com/oberthci/oberth/internal/publicerr"
	"github.com/oberthci/oberth/internal/stateport"
	"github.com/oberthci/oberth/internal/stepmodel"
)

func checkRequestedWorkflow(t *testing.T, f RunFixture) {
	for _, mode := range []string{"admitted", "rejected"} {
		b := batchFor(t, f, RunCase{ID: mode})
		file := b.Runs[0].Prepared.View().SelectedFile
		if mode == "rejected" {
			file = ".oberth/walk.yaml"
			b.Runs[0].Prepared = nil
			b.Runs[0].Rejected = &stateport.RejectedAdmission{ID: mode,
				Failure:  stepmodel.FailureRecord{Class: stepmodel.FailureAuthor, Retry: stepmodel.RetryHint{Action: stepmodel.RetryRerun}},
				Findings: []admissioncontract.Finding{{Code: "missing", Reason: "workflow file is missing", Remedy: "Restore the requested workflow."}}}
		}
		b.Runs[0].Request.RequestedFile = file
		_, err := f.Port.AcceptBatch(t.Context(), b)
		mustRun(t, err)
		read, err := f.Port.Run(t.Context(), mode)
		mustRun(t, err)
		if read.Origin.RequestedFile != file {
			t.Fatal("stored run lost explicit workflow", mode, read.Origin)
		}
		b.Runs[0].Request.RequestedFile = ".oberth/different.yaml"
		if _, err := f.Port.AcceptBatch(t.Context(), b); err == nil {
			t.Fatal("replay replaced requested workflow", mode)
		}
		read, err = f.Port.Run(t.Context(), mode)
		mustRun(t, err)
		if read.Origin.RequestedFile != file {
			t.Fatal("conflicting replay changed stored workflow", mode)
		}
	}
	for _, file := range []string{"../escape.yaml", ".oberth/different.yaml"} {
		b := batchFor(t, f, RunCase{ID: "invalid"})
		b.Runs[0].Request.RequestedFile = file
		_, err := f.Port.AcceptBatch(t.Context(), b)
		runCode(t, err, publicerr.InvalidInput)
	}
	// A rejected admission has no prepared seal, so only the stored-request
	// boundary can refuse an unsafe path here.
	b := batchFor(t, f, RunCase{ID: "invalid-rejected"})
	b.Runs[0].Prepared = nil
	b.Runs[0].Rejected = &stateport.RejectedAdmission{ID: "invalid-rejected",
		Failure:  stepmodel.FailureRecord{Class: stepmodel.FailureAuthor, Retry: stepmodel.RetryHint{Action: stepmodel.RetryRerun}},
		Findings: []admissioncontract.Finding{{Code: "missing", Reason: "workflow file is missing", Remedy: "Restore the requested workflow."}}}
	b.Runs[0].Request.RequestedFile = "../escape.yaml"
	_, err := f.Port.AcceptBatch(t.Context(), b)
	runCode(t, err, publicerr.InvalidInput)
}
