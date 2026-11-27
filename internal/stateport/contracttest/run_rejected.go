package contracttest

import (
	"testing"

	"github.com/oberthci/oberth/internal/admissioncontract"
	"github.com/oberthci/oberth/internal/nativebinding"
	"github.com/oberthci/oberth/internal/publicerr"

	"github.com/oberthci/oberth/internal/stateport"
	"github.com/oberthci/oberth/internal/stepmodel"
)

func checkRejectedAndRerun(t *testing.T, f RunFixture) {
	b := batchFor(t, f, RunCase{ID: "rejected"})
	b.Runs[0].Prepared = nil
	b.Runs[0].Rejected = &stateport.RejectedAdmission{ID: "rejected", Failure: stepmodel.FailureRecord{Class: stepmodel.FailureAuthor, Retry: stepmodel.RetryHint{Action: stepmodel.RetryNone}}, Findings: []admissioncontract.Finding{{Code: "invalid", Reason: "admission refused", Remedy: "Fix the pipeline."}}}
	recorded, err := f.Port.AcceptBatch(t.Context(), b)
	rows := recorded.Runs
	mustRun(t, err)
	if len(rows) != 1 || rows[0].HasAdmission || rows[0].DisplayReason != stateport.ReasonAdmission || len(rows[0].Findings) != 1 {
		t.Fatal("rejected outcome gained admission or lost findings", rows)
	}
	r := acceptOneRun(t, f, RunCase{ID: "source", Ref: "refs/heads/rerun"})
	rerun := batchFor(t, f, RunCase{ID: "rerun", Ref: r.Ref})
	rerun.Runs[0].RerunOf = r.ID
	mustRun(t, f.Port.Finish(t.Context(), stateport.Action{}, stateport.RunUpdate{ID: r.ID, Revision: r.Revision, Status: stepmodel.RunFailed, Restart: true}))
	replayed, err := f.Port.AcceptBatch(t.Context(), rerun)
	rows = replayed.Runs
	mustRun(t, err)
	if len(rows) != 1 || rows[0].Origin.RerunOf != r.ID {
		t.Fatal("rerun lost origin")
	}
}

func checkMalformedAcceptance(t *testing.T, f RunFixture) {
	for _, mode := range []string{"rejected-child", "prepared-and-rejected"} {
		t.Run(mode, func(t *testing.T) {
			b := batchFor(t, f, RunCase{ID: mode})
			b.Runs[0].Rejected = &stateport.RejectedAdmission{ID: mode,
				Failure:  stepmodel.FailureRecord{Class: stepmodel.FailureAuthor, Retry: stepmodel.RetryHint{Action: stepmodel.RetryNone}},
				Findings: []admissioncontract.Finding{{Code: "invalid", Reason: "admission refused", Remedy: "Fix the pipeline."}}}
			if mode == "rejected-child" {
				b.Runs[0].Prepared = nil
				b.Runs[0].Child = &nativebinding.ChildBinding{}
			}
			head := len(f.Audit(t))
			ch := observeChanges(t, f.Port)
			result, err := f.Port.AcceptBatch(t.Context(), b)
			runCode(t, err, publicerr.InvalidInput)
			if result.Fresh || result.Runs != nil || len(f.Audit(t)) != head {
				t.Fatal("invalid admission changed store", result)
			}
			unchangedSignal(t, ch)
			_, err = f.Port.AcceptedRuns(t.Context(), b.ReceiveID)
			runCode(t, err, publicerr.NotFound)
			_, err = f.Port.Run(t.Context(), mode)
			runCode(t, err, publicerr.NotFound)
		})
	}
}
