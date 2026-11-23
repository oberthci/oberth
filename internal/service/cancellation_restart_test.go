package service_test

import (
	"context"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/gitport"
	"github.com/oberthci/oberth/internal/service"
	"github.com/oberthci/oberth/internal/stateport"
	"github.com/oberthci/oberth/internal/stepmodel"
)

func TestCancelledRunNamesCallerOnce(t *testing.T) {
	t.Parallel()
	f := setup(t)
	f.seed(stepmodel.RunRunning)
	f.exec.CancelFunc = func(ctx context.Context, _ stateport.Run, _ bool) error {
		return f.s.Finished(ctx, service.FinishedRun{RunID: runID, Status: stepmodel.RunCancelled})
	}
	if _, err := f.s.Cancel(t.Context(), caller(), actionInput{ID: runID}); err != nil {
		t.Fatal(err)
	}
	got, err := f.store.native.Run(t.Context(), runID)
	want := "Cancelled by " + caller().Identity + ".\nPush a new commit or request a rerun to run again."
	if err != nil || got.Detail != want || len(got.Detail) > 120 {
		t.Fatal("restart guidance repeats cancellation or loses its caller", got.Detail, err)
	}
}

func TestCancelledPromotionExplainsHowToStartAgain(t *testing.T) {
	t.Parallel()
	f := setup(t)
	f.seed(stepmodel.RunPassed)
	merged := strings.Repeat("d", 40)
	f.moving.Prepare = func(_ context.Context, q gitport.PromotionRequest) (gitport.Promotion, error) {
		return gitport.Promotion{Result: gitport.Ref{Name: q.Target.Name, ObjectSHA: merged, CommitSHA: merged}}, nil
	}
	first := promote(t, f)
	f.exec.CancelFunc = func(ctx context.Context, r stateport.Run, _ bool) error {
		return f.s.Finished(ctx, service.FinishedRun{RunID: r.ID, Status: stepmodel.RunCancelled})
	}
	if _, err := f.s.Cancel(t.Context(), admin(), actionInput{ID: first.RunID}); err != nil {
		t.Fatal(err)
	}
	got, err := f.store.native.Run(t.Context(), first.RunID)
	want := "Cancelled by " + admin().Identity + ".\nRequest a new promotion from the original passed run."
	if err != nil || got.Status != stepmodel.RunCancelled || got.Failure != nil || got.Detail != want || len(got.Detail) > 120 {
		t.Fatal("cancelled promotion offers an inapplicable restart", got, err)
	}
	if again := promote(t, f); again.RunID == first.RunID {
		t.Fatal("promotion restart guidance reused the cancelled run", again)
	}
}
