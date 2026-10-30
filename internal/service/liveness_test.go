package service

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/api"
	"github.com/oberthci/oberth/internal/liveness"
	"github.com/oberthci/oberth/internal/model"
)

type stalledClaimStore struct {
	SchedulerStore
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (store *stalledClaimStore) ClaimNextRun(ctx context.Context) (model.Run, error) {
	store.once.Do(func() { close(store.entered) })
	select {
	case <-store.release:
		return store.SchedulerStore.ClaimNextRun(ctx)
	case <-ctx.Done():
		return model.Run{}, ctx.Err()
	}
}

func TestSchedulerLivenessDetectsStalledLocalDispatch(t *testing.T) {
	fixture := newRecoveryFixture(t)
	scheduler := fixture.scheduler(t, &recoveryJobs{})
	// Boundary assertions must not spend their one-second virtual margin on
	// wall-clock scheduling delay between the worker signal and the check.
	now := time.Now()
	scheduler.dispatchProgress.Now = func() time.Time { return now }
	stalled := &stalledClaimStore{SchedulerStore: scheduler.store, entered: make(chan struct{}), release: make(chan struct{})}
	scheduler.store = stalled
	// This wrapper omits optional HeadWaitStore, so this tests dispatch alone.
	if err := scheduler.liveAt(now.Add(24 * time.Hour)); err != nil {
		t.Fatal("startup recovery is not a local wedge", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- scheduler.Run(ctx) }()
	defer func() { cancel(); awaitLivenessDone(t, done) }()
	awaitLivenessSignal(t, stalled.entered)
	if err := scheduler.liveAt(now.Add(liveness.MaxStall - time.Second)); err != nil {
		t.Fatal("short queue wait killed liveness", err)
	}
	if err := scheduler.liveAt(now.Add(liveness.MaxStall + time.Second)); !errors.Is(err, liveness.ErrStalled) || !strings.HasPrefix(err.Error(), "scheduler:") {
		t.Fatalf("stalled dispatch: %v", err)
	}
	close(stalled.release)
	if err := scheduler.Live(); err != nil {
		t.Fatal("dispatch did not recover", err)
	}
}

type stalledHeadStore struct {
	SchedulerStore
	HeadWaitStore
	entered chan struct{}
	release chan struct{}
}

func (store *stalledHeadStore) RecordAdmissionHeadWait(ctx context.Context, _ int64, _ model.RefKind, _, _ string, _ time.Time) error {
	close(store.entered)
	select {
	case <-store.release:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func TestAdmissionLivenessDetectsStalledPersistenceAndStops(t *testing.T) {
	fixture := newRecoveryFixture(t)
	scheduler := fixture.scheduler(t, &recoveryJobs{})
	stalled := &stalledHeadStore{SchedulerStore: scheduler.store, HeadWaitStore: fixture.store, entered: make(chan struct{}), release: make(chan struct{})}
	scheduler.store = stalled
	scheduler.admission.headEvents = make(chan headEvent, 1)
	scheduler.admission.headEvents <- headEvent{key: headWaitKey{RepoID: 1}, since: time.Now()}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { scheduler.runHeadEvents(ctx); done <- nil }()
	defer func() { close(stalled.release); cancel(); awaitLivenessDone(t, done) }()
	awaitLivenessSignal(t, stalled.entered)
	if err := scheduler.liveAt(time.Now().Add(liveness.MaxStall + time.Second)); !errors.Is(err, liveness.ErrStalled) || !strings.HasPrefix(err.Error(), "admission persistence:") {
		t.Fatalf("stalled admission: %v", err)
	}
}

type queuedLivenessPromotions struct {
	PromotionRepository
	calls    int // used by the initial call and then its sole queue worker
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
	// without hiding this worker's lack of local progress.
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

	jobs := &livenessAdmissionJobs{entered: make(chan struct{})}
	scheduler.jobs = jobs
	ctx, cancel = context.WithCancel(t.Context())
	go func() {
		done <- scheduler.awaitDispatchAdmission(withDispatchProgress(ctx, &scheduler.dispatchProgress))
	}()
	awaitLivenessSignal(t, jobs.entered)
	if err := scheduler.dispatchProgress.CheckAt(time.Now().Add(24 * time.Hour)); err != nil {
		t.Fatal("Kubernetes admission determined liveness", err)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(testStartTimeout(5 * time.Second)):
		t.Fatal("admission wait did not stop")
	}
}

type livenessAdmissionJobs struct {
	recoveryJobs
	entered chan struct{}
}

func (jobs *livenessAdmissionJobs) AwaitAdmission(ctx context.Context) error {
	close(jobs.entered)
	<-ctx.Done()
	return ctx.Err()
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

func TestLivenessDispatchPauseSurvivesOtherGoroutine(t *testing.T) {
	fixture := newRecoveryFixture(t)
	jobs := &livenessAdmissionJobs{entered: make(chan struct{})}
	scheduler := fixture.scheduler(t, jobs)
	scheduler.dispatchProgress.Beat()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	dispatchCtx := withDispatchProgress(ctx, &scheduler.dispatchProgress)
	done := make(chan error, 1)
	go func() { done <- scheduler.awaitDispatchAdmission(dispatchCtx) }()
	awaitLivenessSignal(t, jobs.entered)
	otherDone := make(chan struct{})
	go func() {
		defer close(otherDone)
		suspendDispatch(context.WithoutCancel(dispatchCtx))()
		beatDispatch(context.WithoutCancel(dispatchCtx))
	}()
	awaitLivenessSignal(t, otherDone)
	if err := scheduler.dispatchProgress.CheckAt(time.Now().Add(24 * time.Hour)); err != nil {
		t.Fatal("background promotion context cleared dispatch's admission pause", err)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(testStartTimeout(5 * time.Second)):
		t.Fatal("admission did not stop")
	}
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

type idleLivenessStore struct {
	SchedulerStore
	claims atomic.Int64
}

func (store *idleLivenessStore) ClaimNextRun(ctx context.Context) (model.Run, error) {
	run, err := store.SchedulerStore.ClaimNextRun(ctx)
	store.claims.Add(1)
	return run, err
}

type idleLivenessPoint struct {
	claims int64
	resume chan struct{}
}

func TestSchedulerLivenessIdleRunStaysLiveAcrossVirtualHour(t *testing.T) {
	fixture := newRecoveryFixture(t)
	scheduler := fixture.scheduler(t, &recoveryJobs{})
	start := time.Now()
	var elapsed atomic.Int64
	clock := func() time.Time { return start.Add(time.Duration(elapsed.Load())) }
	scheduler.dispatchProgress.Now = clock
	ticks := make(chan time.Time)
	scheduler.dispatchTicks = ticks
	idle := &idleLivenessStore{SchedulerStore: scheduler.store}
	scheduler.store = idle // omit optional head-persistence worker to isolate dispatch
	ctx, cancel := context.WithCancel(t.Context())
	points := make(chan idleLivenessPoint)
	scheduler.onDispatchIdle = func() {
		point := idleLivenessPoint{claims: idle.claims.Load(), resume: make(chan struct{})}
		select {
		case points <- point:
		case <-ctx.Done():
			return
		}
		// Keep virtual time stable throughout the entire dispatch iteration. A
		// claim-return timestamp alone can belong to a cycle whose heartbeat ran
		// before the test advanced time. Acknowledge the actual idle boundary.
		select {
		case <-point.resume:
		case <-ctx.Done():
		}
	}
	done := make(chan error, 1)
	go func() { done <- scheduler.Run(ctx) }()
	defer func() { cancel(); awaitLivenessDone(t, done) }()
	nextPoint := func() idleLivenessPoint {
		select {
		case point := <-points:
			return point
		case <-time.After(testStartTimeout(5 * time.Second)):
			t.Fatal("scheduler did not finish its idle iteration")
			return idleLivenessPoint{}
		}
	}
	var previousClaims int64
	checkPoint := func(point idleLivenessPoint) {
		if point.claims <= previousClaims {
			t.Fatal("idle iteration did not make an actual queue claim")
		}
		previousClaims = point.claims
		if err := scheduler.Live(); err != nil {
			t.Fatalf("idle after %s: %v", time.Duration(elapsed.Load()), err)
		}
	}
	point := nextPoint()
	for step := 0; step <= 10; step++ {
		checkPoint(point)
		if step == 10 {
			close(point.resume)
			break
		}
		// The scheduler is parked at its observed idle boundary, not midway
		// through DB work, when the test advances the clock by more than MaxStall.
		elapsed.Add(int64(6 * time.Minute))
		want := clock()
		close(point.resume)
		deadline := time.NewTimer(testStartTimeout(5 * time.Second))
		sent := false
		for !sent {
			select {
			case ticks <- want:
				sent = true
			case extra := <-points:
				// Startup also queues a wake. Let any such completed iteration pass
				// without moving time again, so it cannot deadlock the tick handshake.
				checkPoint(extra)
				close(extra.resume)
			case <-deadline.C:
				t.Fatal("idle scheduler never polled")
			}
		}
		deadline.Stop()
		point = nextPoint()
	}
}
