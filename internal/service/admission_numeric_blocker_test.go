package service

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/store"
	"github.com/oberthci/oberth/pkg/argoworkflow"
	"github.com/oberthci/oberth/pkg/periapsis"
)

func TestLegacyHeadBlocksOnNumericBudgetBesideDeclaredRun(t *testing.T) {
	g := newWeightedAdmission(8)
	release := acquireRequestWithinBudget(t, g.reserve(1), admissionRequest{Size: periapsis.XL, Pods: &argoworkflow.PodDemand{CPUMilli: 6900, MemoryBytes: 1 << 30, Pods: 1}})
	defer release()
	head := priorityWaiter(g, 2, model.PriorityNormal, time.Now(), periapsis.M)
	defer head.cancel()
	s := &Scheduler{admission: g}
	blocker := s.AdmissionBlocker()
	if blocker == nil || blocker.NumericBudget == nil || !strings.Contains(blocker.Reason(), "CPU") || !strings.Contains(blocker.Reason(), "7000") || !strings.Contains(blocker.Reason(), "8000") {
		t.Fatalf("numeric blocker reported as spare weight: %+v", blocker)
	}
	body, _ := json.Marshal(s.AdmissionState())
	if !strings.Contains(string(body), `"numeric_budget"`) || !strings.Contains(string(body), `"cpu_used_milli":7000`) {
		t.Fatalf("state omits numeric capacity: %s", body)
	}
}

func TestNumericBlockerBudgetIsHiddenFromScopedActors(t *testing.T) {
	budget := &model.NumericBudget{CPUUsedMilli: 7000, CPUBudgetMilli: 8000}
	blocker := &model.AdmissionBlocker{RepoID: 1, NumericBudget: budget}
	api := &API{scope: &repoScope{repos: map[int64]model.Repository{1: {ID: 1}}}}
	visible := api.visibleBlocker(blocker)
	if visible == nil || visible.NumericBudget != nil {
		t.Fatalf("scoped blocker exposes shared numeric usage: %+v", visible)
	}
	if blocker.NumericBudget != budget {
		t.Fatal("scoped presentation changed the original blocker")
	}
	blocker.RepoID = 2
	if api.visibleBlocker(blocker) != nil {
		t.Fatal("scoped actor sees another repository's blocker")
	}
}

func TestNumericBackfillDoesNotConsumeBlockedHeadsBudget(t *testing.T) {
	g := newWeightedAdmission(8)
	g.maxHeadWait = time.Minute
	g.weights.L = 4
	release := acquireRequestWithinBudget(t, g.reserve(1), admissionRequest{Size: periapsis.XL, Pods: &argoworkflow.PodDemand{CPUMilli: 4900, MemoryBytes: 1 << 30, Pods: 1}})
	defer release()
	head := priorityWaiter(g, 2, model.PriorityNormal, time.Now(), periapsis.L)
	defer head.cancel()
	candidate := g.reserve(3)
	defer candidate.cancel()
	g.mu.Lock()
	candidate.prepared, candidate.size = true, periapsis.XL
	candidate.pods = &argoworkflow.PodDemand{CPUMilli: 2400, MemoryBytes: 1 << 30, Pods: 1}
	g.grantLocked()
	admitted := candidate.granted
	g.mu.Unlock()
	if admitted {
		candidate.release()
		t.Fatal("2500m backfill consumed budget needed by the blocked 4000m head")
	}
}

func TestPriorityBumpReevaluatesAvailableBudgetImmediately(t *testing.T) {
	f := newControlFixture(t)
	g := newWeightedAdmission(4)
	f.scheduler.admission = g
	release := acquireRequestWithinBudget(t, g.reserve(-1), admissionRequest{Size: periapsis.M})
	defer release()
	var waiters []*admissionReservation
	for i, size := range []periapsis.Size{periapsis.L, periapsis.S} {
		_, err := f.store.EnqueueRun(t.Context(), model.RunSpec{RepoID: f.repo.ID, RefKind: model.RefBranch, Ref: []string{"head", "small"}[i], SHA: strings.Repeat("a", 40), Actor: "agent", Trigger: "branch"})
		if err != nil {
			t.Fatal(err)
		}
		run, err := f.store.ClaimNextRun(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		r := g.reserveRun(run)
		defer r.cancel()
		g.mu.Lock()
		r.prepared, r.size, r.weight = true, size, g.weights.of(size, g.capacity)
		g.grantLocked()
		g.mu.Unlock()
		waiters = append(waiters, r)
	}
	f.scheduler.store = &observedPriorityStore{Store: f.store, before: func() {
		if !g.mu.TryLock() {
			t.Error("SQLite priority write holds the admission mutex")
			return
		}
		defer g.mu.Unlock()
	}}
	if _, err := f.scheduler.BumpRunPriority(context.Background(), "admin", waiters[1].runID, model.PriorityHighest, "urgent"); err != nil {
		t.Fatal(err)
	}
	if !granted(g, waiters[1]) {
		t.Fatal("bumped run still waits for an unrelated admission event")
	}
	waiters[1].release()
}

type observedPriorityStore struct {
	*store.Store
	before func()
}

func (s *observedPriorityStore) BumpRunPriority(ctx context.Context, actor, id string, priority model.RunPriority, reason string) (model.Run, error) {
	s.before()
	return s.Store.BumpRunPriority(ctx, actor, id, priority, reason)
}

type recordingHeadStore struct {
	SchedulerStore
	HeadWaitStore
	persisted chan headEvent
}

func (s *recordingHeadStore) RecordAdmissionHeadWait(_ context.Context, repo int64, kind model.RefKind, ref, sha string, since time.Time) error {
	s.persisted <- headEvent{key: headWaitKey{repo, kind, ref, sha}, since: since}
	return nil
}

func (s *recordingHeadStore) ClearAdmissionHeadWait(_ context.Context, repo int64, kind model.RefKind, ref, sha string) error {
	s.persisted <- headEvent{key: headWaitKey{repo, kind, ref, sha}, clear: true}
	return nil
}

func TestHeadWaitPersistenceCoalescesWithoutDroppingClear(t *testing.T) {
	g := newWeightedAdmission(8)
	g.headEvents = make(chan headEvent, 1)
	key := headWaitKey{RepoID: 1, RefKind: model.RefBranch, Ref: "main", SHA: strings.Repeat("a", 40)}
	other := key
	other.Ref = "other"
	g.mu.Lock()
	g.emitHeadLocked(headEvent{key: key, since: time.Now()})
	g.emitHeadLocked(headEvent{key: key, clear: true})
	g.emitHeadLocked(headEvent{key: other, since: time.Now()})
	g.mu.Unlock()
	state := &recordingHeadStore{persisted: make(chan headEvent, 4)}
	s := &Scheduler{store: state, admission: g}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { defer close(done); s.runHeadEvents(ctx) }()
	defer func() { cancel(); <-done }()
	seen := map[headWaitKey]headEvent{}
	for len(seen) < 2 {
		select {
		case event := <-state.persisted:
			seen[event.key] = event
		case <-time.After(testTimeout(time.Second)):
			t.Fatalf("full notification channel lost head-wait changes: %+v", seen)
		}
	}
	if !seen[key].clear {
		t.Fatal("stale record overwrote its later clear")
	}
}

func TestHeadWaitPersistencePreservesOriginsAcrossCoalescing(t *testing.T) {
	for _, test := range []struct {
		name             string
		reset, failClear bool
	}{
		{name: "first origin"},
		{name: "clear then new origin", reset: true},
		{name: "failed clear with a newer record", reset: true, failClear: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newControlFixture(t)
			g := newWeightedAdmission(8)
			g.headEvents = make(chan headEvent, 1)
			key := headWaitKey{RepoID: f.repo.ID, RefKind: model.RefBranch, Ref: "main", SHA: strings.Repeat("a", 40)}
			first := time.Now().UTC().Add(-time.Minute)
			next := first.Add(30 * time.Second)
			want := first
			if test.reset {
				if err := f.store.RecordAdmissionHeadWait(t.Context(), key.RepoID, key.RefKind, key.Ref, key.SHA, first); err != nil {
					t.Fatal(err)
				}
				want = next
			}
			g.mu.Lock()
			g.emitHeadLocked(headEvent{key: key, since: first, clear: test.reset})
			g.emitHeadLocked(headEvent{key: key, since: next})
			g.mu.Unlock()
			s := &Scheduler{store: f.store, admission: g}
			if test.failClear {
				failed := false
				s.store = &retryHeadClearStore{Store: f.store, beforeClear: func() error {
					if failed {
						return nil
					}
					failed = true
					g.mu.Lock()
					g.emitHeadLocked(headEvent{key: key, since: next.Add(time.Second)})
					g.mu.Unlock()
					return errors.New("injected persistence failure")
				}}
			}
			ctx, cancel := context.WithCancel(t.Context())
			done := make(chan struct{})
			go func() { defer close(done); s.runHeadEvents(ctx) }()
			defer func() { cancel(); <-done }()
			deadline := time.Now().Add(testTimeout(time.Second))
			for {
				got, found, err := f.store.AdmissionHeadWait(t.Context(), key.RepoID, key.RefKind, key.Ref, key.SHA)
				if err != nil {
					t.Fatal(err)
				}
				if found && got.Equal(want) {
					return
				}
				if time.Now().After(deadline) {
					t.Fatalf("persisted origin = %s (found=%t), want %s", got, found, want)
				}
				time.Sleep(time.Millisecond)
			}
		})
	}
}

type retryHeadClearStore struct {
	*store.Store
	beforeClear func() error
}

func (s *retryHeadClearStore) ClearAdmissionHeadWait(ctx context.Context, repo int64, kind model.RefKind, ref, sha string) error {
	if err := s.beforeClear(); err != nil {
		return err
	}
	return s.Store.ClearAdmissionHeadWait(ctx, repo, kind, ref, sha)
}
