package service

import (
	"context"
	"errors"
	"strings"
	"sync"
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
	stalled := &stalledClaimStore{SchedulerStore: scheduler.store, entered: make(chan struct{}), release: make(chan struct{})}
	scheduler.store = stalled
	// This wrapper omits optional HeadWaitStore, so this tests dispatch alone.
	if err := scheduler.liveAt(time.Now().Add(24 * time.Hour)); err != nil {
		t.Fatal("startup recovery is not a local wedge", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- scheduler.Run(ctx) }()
	defer func() { cancel(); awaitLivenessDone(t, done) }()
	awaitLivenessSignal(t, stalled.entered)
	if err := scheduler.liveAt(time.Now().Add(liveness.MaxStall - time.Second)); err != nil {
		t.Fatal("short queue wait killed liveness", err)
	}
	if err := scheduler.liveAt(time.Now().Add(liveness.MaxStall + time.Second)); !errors.Is(err, liveness.ErrStalled) || !strings.HasPrefix(err.Error(), "scheduler:") {
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
