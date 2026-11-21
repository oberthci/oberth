package service_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"testing/synctest"

	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/gitport"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/service"
	"github.com/oberthci/oberth/internal/stateport"
	"github.com/oberthci/oberth/internal/stepmodel"
)

// movedPromotion is a passed promotion whose publication will find the
// default branch moved by another promotion of the same head; later
// publications are delivered.
func movedPromotion(t *testing.T, f *fixture) model.Promotion {
	t.Helper()
	f.seed(stepmodel.RunPassed)
	preparations, moved := 0, false
	currentHead := strings.Repeat("b", 40)
	f.source.File = func(context.Context, string, string) (gitport.Ref, gitport.File, error) {
		return gitport.Ref{Name: "refs/heads/main", ObjectSHA: currentHead, CommitSHA: currentHead}, gitport.File{Bytes: []byte("uplinks_may_promote: true\n")}, nil
	}
	f.moving.Prepare = func(_ context.Context, q gitport.PromotionRequest) (gitport.Promotion, error) {
		preparations++
		sha := strings.Repeat(string(rune('c'+preparations)), 40)
		return gitport.Promotion{Result: gitport.Ref{Name: q.Target.Name, ObjectSHA: sha, CommitSHA: sha}}, nil
	}
	delivered := f.moving.Push
	f.moving.Push = func(ctx context.Context, p model.Publication) (gitport.PublicationReceipt, error) {
		if !moved {
			moved, currentHead = true, strings.Repeat("e", 40)
			return gitport.PublicationReceipt{}, &gitport.RefMovedError{Repository: repository, Ref: "refs/heads/main"}
		}
		return delivered(ctx, p)
	}
	out := promote(t, f)
	p, err := f.store.Promotion(t.Context(), out.ID)
	if err != nil {
		t.Fatal(err)
	}
	finishPromotionRun(t, f, p, stepmodel.RunPassed)
	return p
}

func finishPromotionRun(t *testing.T, f *fixture, p model.Promotion, status stepmodel.RunStatus) {
	t.Helper()
	if err := f.s.Finished(t.Context(), service.FinishedRun{RunID: p.RunID, Status: status}); err != nil {
		t.Fatal(err)
	}
}

func deliverPromotion(t *testing.T, f *fixture, id string) {
	t.Helper()
	if err := f.s.Deliver(t.Context(), "publication:"+id); err != nil {
		t.Fatal(err)
	}
}

func retestOf(t *testing.T, f *fixture, id string) model.Promotion {
	t.Helper()
	rows, err := f.store.Promotions(t.Context(), stateport.RecordFilter{Limit: 200})
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range rows {
		if p.PredecessorID == id {
			return p
		}
	}
	t.Fatal("retest not recorded")
	return model.Promotion{}
}

// When the default branch moves under a passed promotion, promote_status
// follows the server's retest and ends with the retest's own outcome, whether
// the wait began before the move or after it.
func TestPromotionStatusFollowsTheRetestOnAMovedBranch(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name       string
		waitBefore bool
		retest     stepmodel.RunStatus
	}{
		{"wait before the move, retest passes", true, stepmodel.RunPassed},
		{"wait after the move, retest passes", false, stepmodel.RunPassed},
		{"wait before the move, retest fails", true, stepmodel.RunFailed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) { checkRetestOutcome(t, tc.waitBefore, tc.retest) })
		})
	}
}

func checkRetestOutcome(t *testing.T, waitBefore bool, retestStatus stepmodel.RunStatus) {
	f := setup(t)
	first := movedPromotion(t, f)
	var waiting <-chan apicontract.PromotionWaitResult
	if waitBefore {
		waiting = promotionStatus(t, f, first.ID, 600)
		synctest.Wait()
	}
	deliverPromotion(t, f, first.ID)
	if !waitBefore {
		waiting = promotionStatus(t, f, first.ID, 600)
	}
	synctest.Wait()
	select {
	case early := <-waiting:
		t.Fatal("the waiter was released before the retest finished", early.Promotion, early.Publication)
	default:
	}
	retest := retestOf(t, f, first.ID)
	finishPromotionRun(t, f, retest, retestStatus)
	if retestStatus == stepmodel.RunPassed {
		synctest.Wait()
		deliverPromotion(t, f, retest.ID)
	}
	requireRetestOutcome(t, <-waiting, retest.ID, retestStatus)
}

// A retest that cannot be decided or started ends the promotion as failed:
// the default branch moved, the retest did not start, the cause is the
// detail, and the wait settles instead of reporting a retest that does not exist.
func TestRetestThatCannotStartEndsThePromotion(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		fail  func(*fixture)
		cause string
	}{
		"merge conflict on the moved branch": {func(f *fixture) {
			f.moving.Prepare = func(context.Context, gitport.PromotionRequest) (gitport.Promotion, error) {
				return gitport.Promotion{Conflicts: []string{"pipeline.yaml"}}, nil
			}
		}, "The source conflicts with the current default branch."},
		"run start error": {func(f *fixture) {
			f.starter.Start = func(context.Context, service.StartRequest) (apicontract.Receipt, error) {
				return apicontract.Receipt{}, errors.New("cluster refused the run")
			}
		}, "cluster refused the run"},
	} {
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) { checkRetestNotStarted(t, tc.fail, tc.cause) })
		})
	}
}

func checkRetestNotStarted(t *testing.T, fail func(*fixture), cause string) {
	f := setup(t)
	first := movedPromotion(t, f)
	fail(f)
	if err := f.s.Deliver(t.Context(), "publication:"+first.ID); err == nil {
		t.Fatal("the retest's start error was not noted")
	}
	got := <-promotionStatus(t, f, first.ID, 600)
	if got.TimedOut || got.Promotion == nil || got.Promotion.ID != first.ID || got.Promotion.Status != model.PromotionFailed {
		t.Fatal("a retest that did not start left the promotion unsettled", got.Promotion, got.TimedOut)
	}
	if got.Promotion.Reason != "default branch moved; retest not started" || !strings.Contains(got.Promotion.Detail, cause) {
		t.Fatal("the failure does not say why", got.Promotion.Reason, got.Promotion.Detail)
	}
}

// A stop between the abandonment of a moved promotion's publication and the
// recording of its retest leaves it passed with no retest; the next start's
// recovery runs the same decision, and the promotion settles: on its retest's
// outcome, or failed when the retest cannot start.
func TestStartRecoversAPromotionStoppedBeforeItsRetest(t *testing.T) {
	t.Parallel()
	for name, conflict := range map[string]bool{"retest starts": false, "retest conflicts": true} {
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) { checkStoppedBeforeRetest(t, conflict) })
		})
	}
}

func checkStoppedBeforeRetest(t *testing.T, conflict bool) {
	f := setup(t)
	first := movedPromotion(t, f)
	stopping, stop := context.WithCancel(t.Context())
	push := f.moving.Push
	f.moving.Push = func(ctx context.Context, p model.Publication) (gitport.PublicationReceipt, error) {
		receipt, err := push(ctx, p)
		stop() // the server stops as the forge answers "moved"
		return receipt, err
	}
	_ = f.s.Deliver(stopping, "publication:"+first.ID)
	if p, _ := f.store.Promotion(t.Context(), first.ID); p.Status != model.PromotionPassed || hasRetest(t, f, first.ID) {
		t.Fatal("the stop did not leave the promotion without a retest", p.Status)
	}
	if conflict {
		f.moving.Prepare = func(context.Context, gitport.PromotionRequest) (gitport.Promotion, error) {
			return gitport.Promotion{Conflicts: []string{"pipeline.yaml"}}, nil
		}
	}
	if err := f.s.RecoverPublications(t.Context()); err != nil {
		t.Fatal("recovery stopped the start", err)
	}
	if conflict {
		got := <-promotionStatus(t, f, first.ID, 600)
		if got.TimedOut || got.Promotion.ID != first.ID || got.Promotion.Reason != "default branch moved; retest not started" {
			t.Fatal("the recovered promotion did not settle as failed", got.Promotion, got.TimedOut)
		}
		return
	}
	retest := retestOf(t, f, first.ID)
	waiting := promotionStatus(t, f, first.ID, 600)
	finishPromotionRun(t, f, retest, stepmodel.RunPassed)
	synctest.Wait()
	deliverPromotion(t, f, retest.ID)
	requireRetestOutcome(t, <-waiting, retest.ID, stepmodel.RunPassed)
}

// One row never keeps the server from starting: an abandoned-as-moved
// publication whose promotion cannot be read is noted, and the other rows are
// still recovered.
func TestStartRecoveryGoesOnPastAnUnreadablePromotion(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		f := setup(t)
		first := movedPromotion(t, f)
		orphan := model.Publication{ID: "publication:gone", Repo: repository, Ref: "refs/heads/main", PromotionID: "gone", ObjectSHA: head, CommitSHA: new(head), Actor: caller().Identity}
		if err := f.store.PreparePublication(t.Context(), stateport.Action{Actor: orphan.Actor}, stateport.PublicationIntent{Publication: orphan}); err != nil {
			t.Fatal(err)
		}
		if err := f.store.AbandonPublication(t.Context(), stateport.Action{Actor: orphan.Actor, Reason: "ref moved"}, orphan.ID); err != nil {
			t.Fatal(err)
		}
		stopping, stop := context.WithCancel(t.Context())
		push := f.moving.Push
		f.moving.Push = func(ctx context.Context, p model.Publication) (gitport.PublicationReceipt, error) {
			receipt, err := push(ctx, p)
			stop()
			return receipt, err
		}
		_ = f.s.Deliver(stopping, "publication:"+first.ID)
		if err := f.s.RecoverPublications(t.Context()); err != nil {
			t.Fatal("one unreadable row stopped the start", err)
		}
		if !hasRetest(t, f, first.ID) {
			t.Fatal("the other row was not recovered")
		}
	})
}

func hasRetest(t *testing.T, f *fixture, id string) bool {
	t.Helper()
	rows, err := f.store.Promotions(t.Context(), stateport.RecordFilter{Limit: 200})
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range rows {
		if p.PredecessorID == id {
			return true
		}
	}
	return false
}

func requireRetestOutcome(t *testing.T, got apicontract.PromotionWaitResult, retest string, status stepmodel.RunStatus) {
	t.Helper()
	if got.TimedOut || got.Promotion == nil || got.Promotion.ID != retest {
		t.Fatal("the outcome is not the retest's", got.Promotion)
	}
	if status == stepmodel.RunPassed && publicationStatus(got) != model.PublicationDelivered {
		t.Fatal("the retest's publication is not the outcome", got.Publication)
	}
	if status == stepmodel.RunFailed && (got.Promotion.Status != model.PromotionFailed || got.Publication != nil) {
		t.Fatal("the retest's failure is not the outcome", got.Promotion)
	}
}
