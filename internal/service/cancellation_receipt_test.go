package service_test

import (
	"context"
	"testing"

	"github.com/oberthci/oberth/internal/service"
	"github.com/oberthci/oberth/internal/stateport"
	"github.com/oberthci/oberth/internal/stepmodel"
)

func TestCancelReceiptOnlyOffersEscalationWhileRunning(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		before, after stepmodel.RunStatus
	}{
		{stepmodel.RunQueued, stepmodel.RunQueued},
		{stepmodel.RunRunning, stepmodel.RunRunning},
		{stepmodel.RunQueued, stepmodel.RunRunning},
		{stepmodel.RunRunning, stepmodel.RunCancelled},
	} {
		t.Run(string(tc.before)+"/"+string(tc.after), func(t *testing.T) {
			f := setup(t)
			f.seed(tc.before)
			f.exec.CancelFunc = func(ctx context.Context, _ stateport.Run, _ bool) error {
				if tc.after == stepmodel.RunCancelled {
					return f.s.Finished(ctx, service.FinishedRun{RunID: runID, Status: tc.after})
				}
				r, err := f.store.native.Run(ctx, runID)
				if err != nil {
					return err
				}
				r.Status = tc.after
				f.store.native.SeedRun(r)
				return nil
			}
			out, err := f.s.Cancel(t.Context(), caller(), actionInput{ID: runID})
			want := ""
			if tc.after == stepmodel.RunRunning {
				want = "repeat cancel to skip exit handlers"
			}
			if err != nil || out.State != string(tc.after) || out.Warning != want {
				t.Fatal("cancel receipt gives escalation advice for the wrong state", out, want, err)
			}
		})
	}
}
