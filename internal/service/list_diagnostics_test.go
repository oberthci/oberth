package service

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	clocktesting "k8s.io/utils/clock/testing"

	"github.com/oberthci/oberth/internal/api"
	"github.com/oberthci/oberth/internal/model"
)

func drainListDiagnostics(t *testing.T, s *API) {
	t.Helper()
	deadline := time.Now().Add(testStartTimeout(5 * time.Second))
	for {
		s.listDiagnostics.mu.Lock()
		pending := len(s.listDiagnostics.pending)
		s.listDiagnostics.mu.Unlock()
		if pending == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("diagnostic refresh did not finish")
		}
		time.Sleep(time.Millisecond)
	}
}

func pollDiagnosticList(t *testing.T, s *API, actor api.Actor, filter api.RunFilter, ready func([]model.Run) bool) []model.Run {
	t.Helper()
	deadline := time.Now().Add(testStartTimeout(5 * time.Second))
	for {
		value, err := s.Runs(t.Context(), actor, filter)
		if err != nil {
			t.Fatal(err)
		}
		drainListDiagnostics(t, s)
		rows := value.([]model.Run)
		if ready(rows) {
			return rows
		}
		if time.Now().After(deadline) {
			t.Fatal("diagnostic result did not appear")
		}
	}
}

func TestListDiagnosticsBoundRefreshesAndJoinShutdown(t *testing.T) {
	f := newControlFixture(t)
	s := f.api(t)
	var calls atomic.Int32
	began := make(chan struct{}, listDiagnosticWorkers)
	load := func(ctx context.Context) listDiagnosticValue {
		calls.Add(1)
		began <- struct{}{}
		<-ctx.Done()
		return listDiagnosticValue{}
	}
	for i := range listDiagnosticLimit + 20 {
		run := model.Run{ID: fmt.Sprint(i), UpdatedAt: time.Now()}
		s.listDiagnostic(run, "startup", load)
		s.listDiagnostic(run, "startup", load)
	}
	for range listDiagnosticWorkers {
		<-began
	}
	s.listDiagnostics.mu.Lock()
	workers := s.listDiagnostics.workers
	pending := len(s.listDiagnostics.pending)
	s.listDiagnostics.mu.Unlock()
	if pending != listDiagnosticLimit || workers != listDiagnosticWorkers {
		t.Fatalf("pending=%d workers=%d", pending, workers)
	}
	ctx, cancel := context.WithTimeout(context.Background(), testStartTimeout(time.Second))
	defer cancel()
	if err := s.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != listDiagnosticWorkers {
		t.Fatalf("refreshes=%d", calls.Load())
	}
	s.listDiagnostic(model.Run{ID: "later"}, "startup", load)
	if calls.Load() != listDiagnosticWorkers {
		t.Fatal("refresh began after shutdown")
	}
}

func TestListDiagnosticsSeparateScopesAndRetainObservationAge(t *testing.T) {
	f := newControlFixture(t)
	s := f.api(t)
	fake := clocktesting.NewFakeClock(time.Now())
	s.waitClock = fake
	run := model.Run{ID: "run", UpdatedAt: fake.Now()}
	observed := fake.Now()
	load := func(context.Context) listDiagnosticValue {
		return listDiagnosticValue{scheduling: &model.RunScheduling{ObservedAt: observed, Message: "Native waiting."}}
	}
	s.listDiagnostic(run, "startup", load)
	drainListDiagnostics(t, s)
	scoped, err := s.forActor(t.Context(), api.Actor{Identity: "scoped", Scope: []string{"codeberg"}})
	if err != nil {
		t.Fatal(err)
	}
	hidden := scoped.listDiagnostic(run, "startup", func(context.Context) listDiagnosticValue { return listDiagnosticValue{} })
	if hidden.scheduling != nil {
		t.Fatal("scoped reader reused global admission data")
	}
	drainListDiagnostics(t, s)
	fake.Step(time.Minute)
	blocked := make(chan struct{})
	stale := s.listDiagnostic(run, "startup", func(ctx context.Context) listDiagnosticValue {
		close(blocked)
		<-ctx.Done()
		return listDiagnosticValue{}
	})
	if stale.scheduling == nil || stale.scheduling.ObservedAt != observed || stale.scheduling.AgeSeconds != 60 {
		t.Fatal("cached observation age changed", stale.scheduling)
	}
	<-blocked
	// A new durable attempt/update must not reuse stale diagnostics.
	run.UpdatedAt = fake.Now()
	if got := s.listDiagnostic(run, "startup", func(context.Context) listDiagnosticValue { return listDiagnosticValue{} }); got.scheduling != nil {
		t.Fatal("new update reused previous diagnostics")
	}
}

func TestListDiagnosticsRefreshWholePageWithoutAnotherRead(t *testing.T) {
	f := newControlFixture(t)
	s := f.api(t)
	var calls atomic.Int32
	release := make(chan struct{})
	load := func(ctx context.Context) listDiagnosticValue {
		select {
		case <-release:
			calls.Add(1)
		case <-ctx.Done():
			t.Error("refresh timed out")
		}
		return listDiagnosticValue{}
	}
	for i := range 20 {
		run := model.Run{ID: fmt.Sprint(i)}
		s.listDiagnostic(run, "startup", load)
		s.listDiagnostic(run, "startup", load)
	}
	close(release)
	drainListDiagnostics(t, s)
	if calls.Load() != 20 {
		t.Fatalf("later rows starved or duplicated: %d", calls.Load())
	}
}
