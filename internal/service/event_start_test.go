package service_test

import (
	"testing"
	"testing/synctest"

	"github.com/oberthci/oberth/internal/apicontract"
)

func TestPrePushSnapshotReturnsImmediatelyAndResumesWithoutLoss(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		f := realEventsFixture(t)
		acceptLiveRun(t, f, "refs/heads/old")
		page := immediateEvents(t, f, apicontract.EventsWaitArgs{Snapshot: true, Own: true})
		if page.err != nil || page.result.TimedOut || len(page.result.Events) != 0 || page.result.NextAfterID == 0 {
			t.Fatal("pre-push snapshot replayed history", page)
		}
		// Work may finish before the client can start its background wait.
		run := acceptLiveRun(t, f, "refs/heads/new")
		result := immediateEvents(t, f, apicontract.EventsWaitArgs{AfterID: &page.result.NextAfterID, Own: true})
		assertAcceptedEvent(t, result, run)
	})
}

func TestEmptyPrePushSnapshotDoesNotWait(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		f := realEventsFixture(t)
		page := immediateEvents(t, f, apicontract.EventsWaitArgs{Snapshot: true})
		if page.err != nil || page.result.TimedOut || page.result.NextAfterID != 0 {
			t.Fatal("empty pre-push snapshot waited", page)
		}
	})
}

func immediateEvents(t *testing.T, f *fixture, q apicontract.EventsWaitArgs) liveEventReply {
	t.Helper()
	done := waitLiveEvents(t, f, q)
	synctest.Wait()
	select {
	case page := <-done:
		return page
	default:
		t.Fatal("pre-push snapshot or retained event blocked")
		return liveEventReply{}
	}
}
