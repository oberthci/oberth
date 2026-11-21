package app_test

import (
	"testing"

	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/stateport"
	"github.com/oberthci/oberth/internal/stepmodel"
)

func TestPreparedRerunReplaysTheStoredAction(t *testing.T) {
	t.Parallel()
	f := newMovingFixture(t)
	branch := f.branch(t, stepmodel.RunFailed)
	call, err := f.svc.PrepareRunRerun(t.Context(), movingCaller(), apicontract.RunRerunArgs{ID: branch.ID})
	landedOK(t, err)
	first, err := call(t.Context())
	landedOK(t, err)
	again, err := call(t.Context())
	landedOK(t, err)
	if first.ID == "" || first.ID == branch.ID || first.ID != again.ID {
		t.Fatal("prepared rerun changed run identity", first, again)
	}
	f.assertStoredAction(t, first.ID, 2)
	f.assertExecutions(t, 2, 0)
	r, err := f.db.Run(t.Context(), first.ID)
	landedOK(t, err)
	if r.Origin.RerunOf != branch.ID || r.Admission.CommitSHA != f.git.Commit {
		t.Fatal("rerun lost source run or admitted commit", r)
	}
}

func TestPreparedPromotionReplaysPendingAndCompletedActions(t *testing.T) {
	t.Parallel()
	f := newMovingFixture(t)
	f.branch(t, stepmodel.RunPassed)
	call := f.promote(t)
	first, err := call(t.Context())
	landedOK(t, err)
	again, err := call(t.Context())
	landedOK(t, err)
	samePromotion(t, first, again)
	f.assertStoredAction(t, first.RunID, 2)
	f.assertExecutions(t, 2, 1)
	f.finish(t, first.RunID, stepmodel.RunPassed)
	completed, err := call(t.Context())
	landedOK(t, err)
	samePromotion(t, first, completed)
	if completed.State != string(model.PromotionPassed) {
		t.Fatal("completed replay lost stored verdict", completed)
	}
	f.assertStoredAction(t, first.RunID, 2)
	f.assertExecutions(t, 2, 1)
}

func TestConcurrentPreparedPromotionCreatesOneBoundRun(t *testing.T) {
	t.Parallel()
	f := newMovingFixture(t)
	f.branch(t, stepmodel.RunPassed)
	call := f.promote(t)
	type outcome struct {
		result apicontract.PromoteResult
		err    error
	}
	ready, begin := make(chan struct{}, 2), make(chan struct{})
	done := make(chan outcome, 2)
	for range 2 {
		go func() {
			ready <- struct{}{}
			<-begin
			r, err := call(t.Context())
			done <- outcome{r, err}
		}()
	}
	<-ready
	<-ready
	close(begin)
	first, again := <-done, <-done
	landedOK(t, first.err)
	landedOK(t, again.err)
	samePromotion(t, first.result, again.result)
	f.assertStoredAction(t, first.result.RunID, 2)
	f.assertExecutions(t, 2, 1)
}

func TestPreparedPromotionRetryReplaysItsOwnStoredAction(t *testing.T) {
	t.Parallel()
	f := newMovingFixture(t)
	f.branch(t, stepmodel.RunPassed)
	failed, err := f.promote(t)(t.Context())
	landedOK(t, err)
	f.finish(t, failed.RunID, stepmodel.RunFailed)
	call, err := f.svc.PreparePromoteRetry(t.Context(), movingCaller(), apicontract.PromoteRetryArgs{ID: failed.ID})
	landedOK(t, err)
	first, err := call(t.Context())
	landedOK(t, err)
	again, err := call(t.Context())
	landedOK(t, err)
	samePromotion(t, first, again)
	if first.ID == failed.ID || first.RunID == failed.RunID {
		t.Fatal("explicit retry reused the failed action", first, failed)
	}
	p, err := f.db.PromotionForRun(t.Context(), first.RunID)
	landedOK(t, err)
	if p.PredecessorID != failed.ID {
		t.Fatal("retry lost predecessor", p)
	}
	f.assertStoredAction(t, first.RunID, 2)
	f.assertExecutions(t, 3, 2)
}

func samePromotion(t *testing.T, first, again apicontract.PromoteResult) {
	t.Helper()
	if first.ID == "" || first.RunID == "" || first.ID != again.ID || first.RunID != again.RunID || first.CommitSHA != again.CommitSHA {
		t.Fatal("prepared promotion changed stored identity", first, again)
	}
}

func (f *movingFixture) assertStoredAction(t *testing.T, run string, calls int) {
	t.Helper()
	if len(f.starter.requests) < calls {
		t.Fatal("missing starter requests", f.starter.requests)
	}
	requests := f.starter.requests[len(f.starter.requests)-calls:]
	id := requests[0].Request.ReceiveID
	if id == "" {
		t.Fatal("action omitted its durable identifier")
	}
	for _, q := range requests {
		if q.Request.ReceiveID != id {
			t.Fatal("replay replaced the action identifier", requests)
		}
	}
	rows, err := f.db.AcceptedRuns(t.Context(), id)
	landedOK(t, err)
	if len(rows) != 1 || rows[0].ID != run || rows[0].Origin.WorkflowUID == "" {
		t.Fatal("action identifier does not identify one durable bound run", id, rows)
	}
}

func (f *movingFixture) assertExecutions(t *testing.T, count, promotions int) {
	t.Helper()
	rows, err := f.db.Runs(t.Context(), stateport.RunFilter{Repositories: []string{request().Repository}, Limit: 100})
	landedOK(t, err)
	if len(rows) != count || f.creates.Load() != int32(count) || f.prepares.Load() != int32(count) || len(f.moving.requests) != promotions {
		t.Fatal("replay repeated run, admission, creation or source preparation", len(rows), f.creates.Load(), f.prepares.Load(), len(f.moving.requests))
	}
}
