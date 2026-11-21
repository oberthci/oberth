package service_test

import (
	"testing"
	"testing/synctest"

	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/stepmodel"
)

// promotionStatus starts one promote_status wait and delivers its answer.
func promotionStatus(t *testing.T, f *fixture, id string, timeout int) <-chan apicontract.PromotionWaitResult {
	t.Helper()
	call, err := f.s.PreparePromoteStatus(t.Context(), apicontract.Caller{Actor: caller()}, apicontract.PromoteStatusArgs{ID: id, TimeoutS: timeout})
	if err != nil {
		t.Fatal(err)
	}
	result := make(chan apicontract.PromotionWaitResult, 1)
	go func() {
		v, err := call(t.Context())
		if err != nil {
			t.Error(err)
		}
		result <- v
	}()
	return result
}

func publicationStatus(r apicontract.PromotionWaitResult) model.PublicationStatus {
	if r.Promotion == nil || r.Promotion.Status != model.PromotionPassed || r.Publication == nil {
		return ""
	}
	return r.Publication.Status
}

// A passed promotion is reported once its target branch is published, so a
// fetch that follows the report reads the promoted commit from the cache.
func TestPromotionStatusWaitsForItsPublication(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		f := setup(t)
		f.seed(stepmodel.RunPassed)
		out := promote(t, f)
		if pending := <-promotionStatus(t, f, out.ID, 1); !pending.TimedOut || publicationStatus(pending) != model.PublicationPending {
			t.Fatal("passed promotion reported before its publication", pending)
		}
		waiting := promotionStatus(t, f, out.ID, 600)
		synctest.Wait()
		if !f.timers.Fire() {
			t.Fatal("publication not armed")
		}
		workerOK(t, f)
		delivered := <-waiting
		if delivered.TimedOut || publicationStatus(delivered) != model.PublicationDelivered || delivered.Publication.ObjectSHA != delivered.Promotion.ResultSHA {
			t.Fatal("delivery did not finish the promotion wait", delivered)
		}
	})
}
