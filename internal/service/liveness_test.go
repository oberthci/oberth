package service

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/api"
	"github.com/oberthci/oberth/internal/liveness"
	"github.com/oberthci/oberth/internal/model"
)

type queuedLivenessPromotions struct {
	PromotionRepository
	calls    int // used by the initial call and then its sole queue background task
	entered  chan struct{}
	external bool
}

func (store *queuedLivenessPromotions) ListPromotions(ctx context.Context, _ model.PromotionListFilter) (model.PromotionPage, error) {
	store.calls++
	if store.calls == 1 {
		return model.PromotionPage{Promotions: []model.Promotion{{ID: "ahead", Sequence: 1, TargetRef: "main", Status: model.PromotionPending}}}, nil
	}
	if store.external {
		return model.PromotionPage{}, nil
	}
	close(store.entered)
	<-ctx.Done()
	return model.PromotionPage{}, ctx.Err()
}

func TestPromotionLivenessTracksEachWorkerAndShutdownRemovesIt(t *testing.T) {
	fixture := newControlFixture(t)
	control := fixture.api(t)
	stalled := &queuedLivenessPromotions{PromotionRepository: control.promotions, entered: make(chan struct{})}
	control.promotions = stalled
	_, err := control.planOrQueuePromotion(t.Context(), api.Actor{}, fixture.repo, model.Run{}, "main", model.Promotion{ID: "queued", Sequence: 2, TargetRef: "main"})
	if err != nil {
		t.Fatal(err)
	}
	awaitLivenessSignal(t, stalled.entered)
	// Other queues can remain healthy (e.g. waiting on external witnesses)
	// without hiding this background task's lack of local progress.
	other := &liveness.Heartbeat{}
	other.Suspend()
	control.promotionProgress.Store("other", other)
	if err := control.liveAt(time.Now().Add(liveness.MaxStall + time.Second)); !errors.Is(err, liveness.ErrStalled) {
		t.Fatalf("stalled promotion: %v", err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), testStartTimeout(5*time.Second))
	defer cancel()
	if err := control.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	if _, found := control.promotionProgress.Load("queued"); found {
		t.Fatal("shutdown left a monitored promotion behind")
	}
}

func TestExternalAdmissionAndWitnessWaitsDoNotFailLiveness(t *testing.T) {
	fixture := newRecoveryFixture(t)
	scheduler := fixture.scheduler(t, &recoveryJobs{})
	entered := make(chan struct{})
	scheduler.mutationGate = func(ctx context.Context) error { close(entered); <-ctx.Done(); return ctx.Err() }
	scheduler.dispatchProgress.Beat()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		done <- scheduler.awaitDispatchMutation(withDispatchProgress(ctx, &scheduler.dispatchProgress))
	}()
	awaitLivenessSignal(t, entered)
	if err := scheduler.dispatchProgress.CheckAt(time.Now().Add(24 * time.Hour)); err != nil {
		t.Fatal("external witnesses determined liveness", err)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(testStartTimeout(5 * time.Second)):
		t.Fatal("external wait did not stop")
	}

}

func TestPromotionWitnessWaitDoesNotFailLiveness(t *testing.T) {
	fixture := newControlFixture(t)
	control := fixture.api(t)
	control.promotions = &queuedLivenessPromotions{PromotionRepository: control.promotions, external: true}
	entered := make(chan struct{})
	control.mutationGate = func(ctx context.Context) error { close(entered); <-ctx.Done(); return ctx.Err() }
	_, err := control.planOrQueuePromotion(t.Context(), api.Actor{}, fixture.repo, model.Run{}, "main", model.Promotion{ID: "queued", Sequence: 2, TargetRef: "main"})
	if err != nil {
		t.Fatal(err)
	}
	awaitLivenessSignal(t, entered)
	if err := control.liveAt(time.Now().Add(24 * time.Hour)); err != nil {
		t.Fatal("external promotion gate determined liveness", err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), testStartTimeout(5*time.Second))
	defer cancel()
	if err := control.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
}

func awaitLivenessSignal(t *testing.T, signal <-chan struct{}) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(testStartTimeout(5 * time.Second)):
		t.Fatal("worker did not reach progress boundary")
	}
}

func awaitLivenessDone(t *testing.T, done <-chan error) {
	t.Helper()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(testStartTimeout(5 * time.Second)):
		t.Fatal("worker did not stop")
	}
}

type livenessRemoteGit struct {
	PublicationGit
	entered chan struct{}
	release chan struct{}
	sha     string
}

func (git *livenessRemoteGit) RemoteRef(ctx context.Context, _, _ string) (string, bool, error) {
	close(git.entered)
	select {
	case <-git.release:
		return git.sha, true, nil
	case <-ctx.Done():
		return "", false, ctx.Err()
	}
}

type livenessFinalizationStore struct {
	PublicationStore
	entered chan struct{}
	release chan struct{}
}

func (store *livenessFinalizationStore) FinalizePublication(ctx context.Context, _ string, _ model.PublicationStatus, _ string) (model.PublicationFinalization, error) {
	close(store.entered)
	select {
	case <-store.release:
		return model.PublicationFinalization{}, nil
	case <-ctx.Done():
		return model.PublicationFinalization{}, ctx.Err()
	}
}

func TestPublicationLivenessExcludesGitButDetectsLocalFinalizationStall(t *testing.T) {
	var progress liveness.Heartbeat
	progress.Beat()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	ctx = withDispatchProgress(ctx, &progress)
	git := &livenessRemoteGit{entered: make(chan struct{}), release: make(chan struct{}), sha: strings.Repeat("a", 40)}
	store := &livenessFinalizationStore{entered: make(chan struct{}), release: make(chan struct{})}
	done := make(chan error, 1)
	go func() {
		_, err := deliverPublication(ctx, store, git, nil, model.Repository{ID: 1, Name: "demo", DefaultBranch: "main"}, "demo",
			model.Publication{ID: "publication", RepoID: 1, RefKind: model.RefBranch, Ref: "feature", Status: model.PublicationPending, ResultSHA: git.sha})
		done <- err
	}()
	awaitLivenessSignal(t, git.entered)
	if err := progress.CheckAt(time.Now().Add(24 * time.Hour)); err != nil {
		t.Fatal("git outage killed liveness", err)
	}
	close(git.release)
	awaitLivenessSignal(t, store.entered)
	if !errors.Is(progress.CheckAt(time.Now().Add(liveness.MaxStall+time.Second)), liveness.ErrStalled) {
		t.Fatal("git exemption hid subsequent local DB stall")
	}
	close(store.release)
	awaitLivenessDone(t, done)
}

type livenessRetryHook func(context.Context, model.Promotion)

func (hook livenessRetryHook) RetryMovedPromotion(ctx context.Context, promotion model.Promotion) {
	hook(ctx, promotion)
}

func TestLivenessMovedPromotionHandoffStripsDispatchCapability(t *testing.T) {
	var heartbeat liveness.Heartbeat
	heartbeat.Beat()
	resume := heartbeat.Suspend()
	defer resume()
	scheduler := &Scheduler{}
	called := false
	scheduler.SetPromotionRetrier(livenessRetryHook(func(ctx context.Context, _ model.Promotion) {
		called = true
		if value, ok := ctx.Value(dispatchProgressKey{}).(*liveness.Heartbeat); ok && value != nil {
			t.Fatal("retry inherited dispatch heartbeat")
		}
		detached := context.WithoutCancel(ctx)
		suspendDispatch(detached)()
		beatDispatch(detached)
	}))
	scheduler.retryMovedPromotion(withDispatchProgress(t.Context(), &heartbeat), model.PublicationFinalization{
		Promotion:   model.Promotion{ID: "moved", Status: model.PromotionFailed},
		Publication: model.Publication{Status: model.PublicationFailed}, TargetMovedTo: strings.Repeat("a", 40),
	})
	if !called {
		t.Fatal("retry handoff not exercised")
	}
	if err := heartbeat.CheckAt(time.Now().Add(24 * time.Hour)); err != nil {
		t.Fatal("detached handoff altered dispatch", err)
	}
	// A typed nil must be as harmless as a stripped key.
	nilCtx := withDispatchProgress(t.Context(), nil)
	suspendDispatch(nilCtx)()
	beatDispatch(nilCtx)
}

func TestSchedulerLivenessIdleRunStaysLiveAcrossVirtualHour(t *testing.T) {
	f := newRecoveryFixture(t)
	scheduler := f.scheduler(t, &recoveryJobs{})
	start := time.Now()
	var elapsed atomic.Int64
	scheduler.dispatchProgress.Now = func() time.Time { return start.Add(time.Duration(elapsed.Load())) }
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- scheduler.runAcceptedFixtures(ctx) }()
	defer func() { cancel(); awaitLivenessDone(t, done) }()
	for !scheduler.Recovered() {
		select {
		case err := <-done:
			t.Fatal(err)
		case <-time.After(time.Millisecond):
		}
	}
	// Read the heartbeat's own synchronized state only after Run has parked.
	for i := 0; i < 100; i++ {
		time.Sleep(time.Millisecond)
		elapsed.Store(int64(time.Hour))
		if scheduler.Live() == nil {
			return
		}
	}
	t.Fatal("idle observer liveness expired")
}
