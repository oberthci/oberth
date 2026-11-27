package service_test

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/publicerr"
	"github.com/oberthci/oberth/internal/service"
	"github.com/oberthci/oberth/internal/stateport"
)

func event(i int) model.AgentEvent {
	return model.AgentEvent{Kind: "run.updated", Repo: repository, RunID: runID, Ref: fmt.Sprintf("refs/heads/topic%d", i)}
}
func TestEventZeroCursorAfterPruneStartsAtOldest(t *testing.T) {
	t.Parallel()
	f := setup(t)
	for i := range 4 {
		f.store.native.SeedEvent(event(i))
	}
	f.store.native.PruneBefore(3)
	p, err := f.s.EventsSince(t.Context(), caller(), eventsInput{})
	if err != nil || p.Gap || p.OldestID != 3 || len(p.Events) != 2 || p.NextAfterID != 4 {
		t.Fatalf("zero cursor %+v %v", p, err)
	}
}
func TestEventsWaitReturnsGapImmediately(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		f := setup(t)
		for i := range 4 {
			f.store.native.SeedEvent(event(i))
		}
		f.store.native.PruneBefore(3)
		p, err := f.s.EventsWait(t.Context(), caller(), eventsInput{After: 1, Timeout: time.Minute, Kinds: []model.EventKind{model.EventRunFinished}})
		if err != nil || !p.Gap || p.TimedOut || p.NextAfterID != 4 {
			t.Fatalf("gap %+v %v", p, err)
		}
	})
}
func TestEventCursorAdvancesPastFilterMisses(t *testing.T) {
	t.Parallel()
	f := setup(t)
	for i := range 501 {
		f.store.native.SeedEvent(event(i))
	}
	p, err := f.s.EventsSince(t.Context(), caller(), eventsInput{Kinds: []model.EventKind{model.EventRunFinished}})
	if err != nil || len(p.Events) != 0 || p.NextAfterID != 501 || p.HasMore {
		t.Fatalf("filtered cursor %+v %v", p, err)
	}
	p, err = f.s.EventsSince(t.Context(), caller(), eventsInput{After: p.NextAfterID, Kinds: []model.EventKind{model.EventRunFinished}})
	if err != nil || p.NextAfterID != 501 || p.HasMore {
		t.Fatalf("continue %+v %v", p, err)
	}
}
func TestEventPageLimitAndContinuation(t *testing.T) {
	t.Parallel()
	f := setup(t)
	for i := range 201 {
		f.store.native.SeedEvent(event(i))
	}
	p, err := f.s.EventsSince(t.Context(), caller(), eventsInput{Limit: 200})
	if err != nil || len(p.Events) != 200 || p.NextAfterID != 200 || !p.HasMore {
		t.Fatalf("page %+v %v", p, err)
	}
	p, err = f.s.EventsSince(t.Context(), caller(), eventsInput{After: p.NextAfterID})
	if err != nil || len(p.Events) != 1 || p.HasMore {
		t.Fatalf("next %+v %v", p, err)
	}
}
func TestOwnEventsOneQueryNoRunReads(t *testing.T) {
	t.Parallel()
	f := setup(t)
	f.seed("running")
	for i := range 500 {
		f.store.native.SeedEvent(event(i))
	}
	before := f.store.Calls()
	p, err := f.s.EventsSince(t.Context(), caller(), eventsInput{Own: true})
	after := f.store.Calls()
	if err != nil || len(p.Events) != 200 || after["Events"]-before["Events"] != 1 || after["Run"] != before["Run"] || after["ResolveRun"] != before["ResolveRun"] {
		t.Fatalf("own %+v %v %v -> %v", p, err, before, after)
	}
}
func TestOwnEventsExcludeOtherActor(t *testing.T) {
	t.Parallel()
	f := setup(t)
	r := f.seed("running")
	r.Origin.Actor = "other"
	f.store.native.SeedRun(r)
	f.store.native.SeedEvent(event(0))
	p, err := f.s.EventsSince(t.Context(), caller(), eventsInput{Own: true})
	if err != nil || len(p.Events) != 0 {
		t.Fatalf("other actor %+v %v", p, err)
	}
}
func TestEventBareRepositoriesAndRunPrefixes(t *testing.T) {
	t.Parallel()
	f := setup(t)
	f.seed("running")
	f.store.native.SeedEvent(event(0))
	p, err := f.s.EventsSince(t.Context(), caller(), eventsInput{Repos: []string{"widget"}, RunIDs: []string{runID[:8]}, RefsGlob: []string{"refs/heads/*"}})
	if err != nil || len(p.Events) != 1 {
		t.Fatalf("selectors %+v %v", p, err)
	}
}
func TestEventHiddenScopeAndPrefix(t *testing.T) {
	t.Parallel()
	f := setup(t)
	r := f.seed("running")
	r.Repo = "private/team/widget"
	f.store.native.SeedRun(r)
	e := event(0)
	e.Repo = r.Repo
	f.store.native.SeedEvent(e)
	p, err := f.s.EventsSince(t.Context(), caller(), eventsInput{})
	if err != nil || len(p.Events) != 0 {
		t.Fatal("hidden event", p, err)
	}
	_, err = f.s.PrepareEventsWait(t.Context(), apicontract.Caller{Actor: caller()}, apicontract.EventsWaitArgs{RunIDs: []string{runID[:8]}})
	requireCode(t, err, publicerr.NotFound)
}

func TestEventInvalidFilters(t *testing.T) {
	t.Parallel()
	f := setup(t)
	for _, q := range []eventsInput{{Kinds: []model.EventKind{"unknown"}}, {RefsGlob: []string{"["}}, {Repos: []string{"../bad"}}, {RunIDs: []string{"short"}}} {
		var err error
		if q.Timeout != 0 {
			_, err = f.s.PrepareEventsWait(t.Context(), apicontract.Caller{Actor: caller()}, apicontract.EventsWaitArgs{TimeoutS: seconds(q.Timeout)})
		} else {
			_, err = f.s.EventsSince(t.Context(), caller(), q)
		}
		requireCode(t, err, publicerr.InvalidInput)
	}
}
func TestRetiredEventHandlersAreAbsent(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"PrepareEventsSince", "PrepareEventsSubscribe"} {
		if _, ok := reflect.TypeFor[*service.Service]().MethodByName(name); ok {
			t.Fatal("retired event handler remains", name)
		}
	}
}

type eventHook struct {
	service.EventStore
	read func(context.Context, stateport.EventFilter) (model.EventPage, error)
}

func (h eventHook) Events(ctx context.Context, q stateport.EventFilter) (model.EventPage, error) {
	return h.read(ctx, q)
}
func TestEventsRegisterBeforeRead(t *testing.T) {
	t.Parallel()
	f := setup(t)
	f.seed("running")
	read := make(chan struct{})
	release := make(chan struct{})
	first := true
	f.c.Events = eventHook{EventStore: f.store, read: func(ctx context.Context, q stateport.EventFilter) (model.EventPage, error) {
		p, err := f.store.Events(ctx, q)
		if first {
			first = false
			close(read)
			<-release
		}
		return p, err
	}}
	f.restart(t)
	done := make(chan apicontract.EventResult, 1)
	errs := make(chan error, 1)
	go func() {
		p, err := f.s.EventsWait(t.Context(), caller(), eventsInput{Timeout: time.Minute})
		errs <- err
		done <- p
	}()
	receive(t, read)
	if err := f.s.Finished(t.Context(), service.FinishedRun{RunID: runID, Status: "passed"}); err != nil {
		t.Fatal(err)
	}
	close(release)
	if err := receive(t, errs); err != nil {
		t.Fatal(err)
	}
	p := receive(t, done)
	if len(p.Events) != 1 || p.Events[0].Kind != string(model.EventRunFinished) || p.TimedOut {
		t.Fatalf("lost wakeup %+v", p)
	}
	if f.store.Calls()["Current"] < 3 {
		t.Fatal("wait omitted authority checks before reread and delivery")
	}
}
func TestEventsCapturedUpperCursor(t *testing.T) {
	t.Parallel()
	f := setup(t)
	f.store.native.SeedEvent(event(0))
	f.c.Events = eventHook{EventStore: f.store, read: func(ctx context.Context, q stateport.EventFilter) (model.EventPage, error) {
		p, err := f.store.Events(ctx, q)
		f.store.native.SeedEvent(event(1))
		return p, err
	}}
	f.restart(t)
	p, err := f.s.EventsSince(t.Context(), caller(), eventsInput{})
	if err != nil || len(p.Events) != 1 || p.LatestID != 1 || p.NextAfterID != 1 {
		t.Fatalf("upper cursor %+v %v", p, err)
	}
}
func TestEventReasonIsCompletePhrase(t *testing.T) {
	t.Parallel()
	f := setup(t)
	f.seed("running")
	if err := f.s.Finished(t.Context(), service.FinishedRun{RunID: runID, Status: "failed"}); err != nil {
		t.Fatal(err)
	}
	p, _ := f.s.EventsSince(t.Context(), caller(), eventsInput{})
	if len(p.Events) != 1 || strings.Contains(p.Events[0].Summary, "...") {
		t.Fatal(p)
	}
}

func TestPrunedHiddenEventsDoNotCauseVisibleGap(t *testing.T) {
	t.Parallel()
	f := setup(t)
	f.store.native.SeedEvent(event(0))
	e := event(1)
	e.Repo = "private/team/hidden"
	f.store.native.SeedEvent(e)
	f.store.native.PruneBefore(3)
	p, err := f.s.EventsSince(t.Context(), caller(), eventsInput{After: 1})
	if err != nil || p.Gap || p.LatestID != 1 {
		t.Fatalf("hidden retention leaked %+v %v", p, err)
	}
}
func TestEmptyRetainedHistoryHasGapForNonzeroCursor(t *testing.T) {
	t.Parallel()
	f := setup(t)
	for i := range 4 {
		f.store.native.SeedEvent(event(i))
	}
	f.store.native.PruneBefore(5)
	p, err := f.s.EventsSince(t.Context(), caller(), eventsInput{After: 1})
	if err != nil || !p.Gap || p.NextAfterID != 4 {
		t.Fatalf("pruned history %+v %v", p, err)
	}
}
func TestEventFiltersRejectMisses(t *testing.T) {
	t.Parallel()
	f := setup(t)
	r := f.seed("running")
	r.ID = "bbbbbbbb000000000000000000000002"
	f.store.native.SeedRun(r)
	for i := range 4 {
		e := event(i)
		if i == 1 {
			e.Kind = "run.finished"
		}
		if i == 2 {
			e.RunID = r.ID
		}
		if i == 3 {
			e.Ref = "refs/tags/check"
		}
		f.store.native.SeedEvent(e)
	}
	for _, tc := range []struct {
		q eventsInput
		n int
	}{{eventsInput{RunIDs: []string{runID}}, 3}, {eventsInput{Kinds: []model.EventKind{model.EventRunFinished}}, 1}, {eventsInput{RefsGlob: []string{"refs/heads/*"}}, 3}} {
		p, err := f.s.EventsSince(t.Context(), caller(), tc.q)
		if err != nil || len(p.Events) != tc.n {
			t.Fatalf("filter %+v %v", p, err)
		}
	}
}
