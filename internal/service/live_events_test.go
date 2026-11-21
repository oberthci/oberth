package service_test

import (
	"testing"
	"testing/synctest"
	"time"

	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/stateport"
)

func TestEventsWaitTimeoutCursorResumesAfterScannedEvents(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		f := realEventsFixture(t)
		acceptLiveRun(t, f, "refs/heads/topic")
		before, err := f.c.Events.Events(t.Context(), stateport.EventFilter{Repositories: []string{repository}, Limit: 200})
		if err != nil || before.LatestID == 0 {
			t.Fatal("missing retained history", before, err)
		}
		done := waitLiveEvents(t, f, apicontract.EventsWaitArgs{RefsGlob: []string{"refs/heads/topic"}, TimeoutS: 25})
		synctest.Wait()
		acceptLiveRun(t, f, "refs/heads/other")
		synctest.Wait()
		page := timeoutLiveEvents(t, done)
		if page.err != nil || !page.result.TimedOut || page.result.Events == nil || len(page.result.Events) != 0 || page.result.NextAfterID <= before.LatestID || page.result.NextAfterID != page.result.LatestID {
			t.Fatal("timeout lost the scanned cursor", page)
		}
		t.Logf("25s timeout advanced cursor from %d to %d", before.LatestID, page.result.NextAfterID)
		cursor := page.result.NextAfterID
		done = waitLiveEvents(t, f, apicontract.EventsWaitArgs{AfterID: &cursor, TimeoutS: 25})
		synctest.Wait()
		run := acceptLiveRun(t, f, "refs/heads/topic")
		synctest.Wait()
		page = receive(t, done)
		assertAcceptedEvent(t, page, run)
		if len(page.result.Events) != 1 || page.result.Events[0].ID <= cursor {
			t.Fatal("resuming a timeout replayed history or skipped the new run", page)
		}
	})
}

func TestAcceptedRunReachesWaitingEventsClientWithinFiveSeconds(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		f := realEventsFixture(t)
		done := waitLiveEvents(t, f, apicontract.EventsWaitArgs{TimeoutS: 25})
		synctest.Wait()
		select {
		case page := <-done:
			t.Fatal("client was not waiting before acceptance", page)
		default:
		}
		deadline := time.NewTimer(5 * time.Second)
		defer deadline.Stop()
		run := acceptLiveRun(t, f, "refs/heads/topic")
		synctest.Wait()
		var page liveEventReply
		select {
		case page = <-done:
		default:
			t.Fatal("committed acceptance did not wake the waiting client")
		}
		assertAcceptedEvent(t, page, run)
		select {
		case <-deadline.C:
			t.Fatal("new run took five seconds to reach its observer")
		default:
		}
		t.Log("acceptance woke the client before advancing the five-second virtual deadline")
		call, err := f.s.PrepareRunGet(t.Context(), workerCaller(), apicontract.RunGetArgs{ID: run.ID})
		snapshot, err := execute(t.Context(), call, err)
		if err != nil || snapshot.ID != run.ID || snapshot.Repo != run.Repo {
			t.Fatal("event arrived before the run could be read", snapshot, err)
		}
	})
}

type liveEventReply struct {
	result apicontract.EventResult
	err    error
}

func timeoutLiveEvents(t *testing.T, done <-chan liveEventReply) liveEventReply {
	t.Helper()
	timer := time.NewTimer(24 * time.Second)
	defer timer.Stop()
	<-timer.C
	synctest.Wait()
	select {
	case page := <-done:
		t.Fatal("wait completed before its 25-second timeout", page)
	default:
	}
	timer.Reset(time.Second)
	<-timer.C
	synctest.Wait()
	select {
	case page := <-done:
		return page
	default:
		t.Fatal("wait exceeded its 25-second timeout")
		return liveEventReply{}
	}
}

func realEventsFixture(t *testing.T) *fixture {
	t.Helper()
	f := setup(t)
	state := landedStore(t, f)
	landedRepository(t, state)
	f.c.Runs, f.c.Catalog, f.c.Events = state, state, state
	f.restart(t)
	return f
}

func waitLiveEvents(t *testing.T, f *fixture, q apicontract.EventsWaitArgs) <-chan liveEventReply {
	t.Helper()
	call, err := f.s.Bindings().EventsWait.Handler.Prepare(t.Context(), workerCaller(), q)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan liveEventReply, 1)
	go func() {
		result, err := call(t.Context())
		done <- liveEventReply{result, err}
	}()
	return done
}

func acceptLiveRun(t *testing.T, f *fixture, ref string) stateport.Run {
	t.Helper()
	id := f.nextID()
	request := model.CIRequest{ReceiveID: "live:" + id, Repo: repository, Ref: ref, ObjectSHA: head, CommitSHA: new(head), Actor: caller(), Trigger: model.TriggerBranch}
	// The producer supplies no event: acceptance must add its own run.updated.
	b := stateport.BatchAcceptance{ReceiveID: request.ReceiveID, Repository: repository, Actor: request.Actor, Runs: []stateport.Acceptance{{Request: request, Prepared: landedAdmission(t, id, request)}}}
	result, err := f.s.Accept(t.Context(), b)
	if err != nil || !result.Fresh || len(result.Runs) != 1 {
		t.Fatal("new run was not accepted", result, err)
	}
	return result.Runs[0]
}

func assertAcceptedEvent(t *testing.T, page liveEventReply, run stateport.Run) {
	t.Helper()
	if page.err != nil || page.result.TimedOut || len(page.result.Events) != 1 {
		t.Fatal("accepted run was not observed", page)
	}
	event := page.result.Events[0]
	if event.Kind != string(model.EventRunUpdated) || event.RunID != run.ID || event.Repo != run.Repo || event.Ref != run.Ref || page.result.NextAfterID != event.ID {
		t.Fatal("accepted event lost run identity or cursor", event, run, page.result)
	}
	assertAcceptedRevision(t, event, run)
}

func assertAcceptedRevision(t *testing.T, event model.AgentEvent, run stateport.Run) {
	t.Helper()
	if event.ObjectSHA != run.ObjectSHA || event.CommitSHA == nil || *event.CommitSHA != *run.CommitSHA {
		t.Fatal("accepted event lost the exact revision", event, run)
	}
}
