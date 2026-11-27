package service_test

import (
	"context"
	"fmt"
	"testing"
	"testing/synctest"
	"time"

	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/model"
)

func TestScopedWaitCoverageReauthorizesWithoutAnEvent(t *testing.T) {
	t.Parallel()
	for _, events := range []bool{false, true} {
		t.Run(fmt.Sprint("events-", events), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) { checkWaitRevocation(t, events) })
		})
	}
}

func checkWaitRevocation(t *testing.T, events bool) {
	t.Helper()
	f := setup(t)
	f.seed("running")
	done := make(chan error, 1)
	go func() {
		if events {
			result, err := f.s.EventsWait(t.Context(), caller(), eventsInput{RunIDs: []string{runID}, Timeout: time.Hour})
			if len(result.Events) > 0 {
				t.Error("revocation delivered cached events")
			}
			done <- err
			return
		}
		result, err := f.s.RunWait(t.Context(), caller(), viewQuery{ID: runID}, time.Hour)
		if result.Run != nil {
			t.Error("revocation delivered cached run")
		}
		done <- err
	}()
	synctest.Wait()
	f.store.mu.Lock()
	a := f.store.Actors[caller().Fingerprint]
	a.Scope = []string{"private"}
	f.store.Actors[caller().Fingerprint] = a
	f.store.mu.Unlock()
	err := f.store.native.RecordAction(t.Context(), model.AuditAction{Actor: a.Identity, Action: "scope.changed", ResourceType: "key", ResourceID: a.Fingerprint})
	if err != nil {
		t.Fatal(err)
	}
	synctest.Wait()
	if err := receive(t, done); err == nil {
		t.Fatal("wait ignored authority revocation without a run event")
	}
}

func TestScopedEventWaitsReadOnlyMatchingRuns(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		f := setup(t)
		r := f.seed("running")
		r.ID = "bbbbbbbb000000000000000000000002"
		f.store.native.SeedRun(r)
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		done := make(chan struct{}, 100)
		for i := range 100 {
			id := r.ID
			if i < 10 {
				id = runID
			}
			go func() {
				_, _ = f.s.EventsWait(ctx, caller(), eventsInput{RunIDs: []string{id}, Timeout: time.Hour})
				done <- struct{}{}
			}()
		}
		synctest.Wait()
		before := f.store.Calls()["Events"]
		f.store.native.SeedEvent(event(0))
		synctest.Wait()
		if reads := f.store.Calls()["Events"] - before; reads != 10 {
			t.Fatal("unrelated event wait reread", reads)
		}
		cancel()
		synctest.Wait()
		for range 100 {
			<-done
		}
	})
}

func TestPreparedWaitRechecksTerminalRunAuthority(t *testing.T) {
	t.Parallel()
	f := setup(t)
	f.seed("passed")
	call, err := f.s.PrepareRunWait(t.Context(), apicontract.Caller{Actor: caller()}, apicontract.RunWaitArgs{ID: runID})
	if err != nil {
		t.Fatal(err)
	}
	f.store.mu.Lock()
	delete(f.store.Actors, caller().Fingerprint)
	f.store.mu.Unlock()
	result, err := call(t.Context())
	if err == nil || result.Run != nil {
		t.Fatal("prepared terminal reply bypassed revocation", result, err)
	}
}

func TestScopedFilterLimitIsBoundedBeforeSubscribe(t *testing.T) {
	t.Parallel()
	f := setup(t)
	_, err := f.s.PrepareEventsWait(t.Context(), apicontract.Caller{Actor: caller()}, apicontract.EventsWaitArgs{RunIDs: make([]string, 33)})
	if err == nil {
		t.Fatal("unbounded subscription keys accepted")
	}
}
