package service

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/store"
	"github.com/oberthci/oberth/pkg/periapsis"
)

const adoptionBaseSHA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
const adoptionTestedSHA = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
const adoptionTagObjectSHA = "cccccccccccccccccccccccccccccccccccccccc"

func slice2ClaimedRun(t *testing.T, kind string) (*recoveryFixture, model.Run, model.Promotion) {
	t.Helper()
	fixture := newRecoveryFixture(t)
	ctx := context.Background()
	spec := model.RunSpec{RepoID: fixture.repo.ID, RefKind: model.RefTag, Ref: "v1.2.3", SHA: adoptionTagObjectSHA, TestedSHA: adoptionTestedSHA, Actor: "agent@host", Trigger: "tag", Release: true, Credentialed: true}
	if kind == "release" {
		spec.Trigger = "release"
		spec.Ref = "v2.0.0"
	}
	var enqueued model.EnqueueRunResult
	var promotion model.Promotion
	var err error
	if kind == "promotion" {
		spec.RefKind = model.RefBranch
		spec.Ref = "promotion/main/bbbbbbbbbbbb"
		spec.SHA = adoptionTestedSHA
		spec.BaseSHA = adoptionBaseSHA
		spec.Trigger = "promotion"
		spec.Release = false
		enqueued, promotion, err = fixture.store.EnqueuePromotionRun(ctx, spec, model.PromotionSpec{RepoID: fixture.repo.ID, SourceBranch: "feature/adopt", SourceSHA: spec.SHA, TargetRef: "main", PreviousSHA: spec.BaseSHA, ResultSHA: spec.SHA, Actor: spec.Actor})
	} else {
		enqueued, err = fixture.store.EnqueueRun(ctx, spec)
	}
	if err != nil {
		t.Fatal(err)
	}
	run, err := fixture.store.ClaimNextRun(ctx)
	if err != nil || run.ID != enqueued.ID {
		t.Fatalf("claim: %+v %v", run, err)
	}
	return fixture, run, promotion
}

func slice2RecoveryRun(t *testing.T, kind string) (*recoveryFixture, model.Run, model.Promotion) {
	t.Helper()
	fixture, run, promotion := slice2ClaimedRun(t, kind)
	ctx := context.Background()
	var err error

	if err = fixture.store.SetRunConcurrencyGroup(ctx, run.ID, "publish"); err != nil {
		t.Fatal(err)
	}
	if _, err = fixture.store.MarkRunAdmitted(ctx, run.ID); err != nil {
		t.Fatal(err)
	}
	fixture.setJobName(t, run.ID, "oberth-"+kind+"-adopt")
	run, err = fixture.store.MarkRunSubmitted(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	return fixture, run, promotion
}
func reopenSlice2Owner(t *testing.T, fixture *recoveryFixture) {
	t.Helper()
	if err := fixture.store.Close(); err != nil {
		t.Fatal(err)
	}
	var err error
	fixture.store, err = store.Open(context.Background(), filepath.Join(fixture.root, "oberth.sqlite"), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = fixture.store.Close() })
	if err = fixture.store.RecoverOwnerState(context.Background()); err != nil {
		t.Fatal(err)
	}
	if cancellations, err := fixture.store.PendingRunCancellations(context.Background()); err != nil || len(cancellations) != 0 {
		t.Fatalf("restart authorized Workflow deletion: %+v %v", cancellations, err)
	}
}
func slice2Scheduler(t *testing.T, fixture *recoveryFixture, jobs *adoptingJobs, git *controlGit) *Scheduler {
	t.Helper()
	scheduler := fixture.scheduler(t, jobs)
	scheduler.git = git
	scheduler.releaseJobs = jobs
	return scheduler
}
func waitSlice2Worker(t *testing.T, done <-chan error, entered <-chan struct{}) {
	t.Helper()
	select {
	case <-entered:
	case err := <-done:
		t.Fatalf("scheduler stopped before Wait: %v", err)
	case <-time.After(testTimeout(5 * time.Second)):
		t.Fatal("adopted Wait not entered")
	}
}
func slice2Workspaces(t *testing.T, fixture *recoveryFixture, run model.Run, promotion model.Promotion) []string {
	t.Helper()
	paths := []string{filepath.Join(fixture.root, "work", run.ID)}
	if promotion.ID != "" {
		paths = append(paths, filepath.Join(fixture.root, "work", "promotion-"+promotion.ID))
	}
	for _, path := range paths {
		if err := os.MkdirAll(filepath.Join(path, "src"), 0750); err != nil {
			t.Fatal(err)
		}
	}
	return paths
}
func waitSlice2Cleanup(t *testing.T, paths []string) {
	t.Helper()
	deadline := time.Now().Add(testTimeout(5 * time.Second))
	for {
		remaining := false
		for _, path := range paths {
			if _, err := os.Stat(path); err == nil {
				remaining = true
			} else if !errors.Is(err, os.ErrNotExist) {
				t.Fatal(err)
			}
		}
		if !remaining {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("terminal workspaces retained", paths)
		}
		time.Sleep(testTimeout(time.Millisecond))
	}
}

// slice2Owner joins every scheduler before its fixture store can close, even
// when an assertion fails. The closed latch also exposes worker errors without
// consuming a one-shot result needed by teardown.
type slice2Owner struct {
	cancel   context.CancelFunc
	done     chan struct{}
	err      error
	stopOnce sync.Once
}

func startSlice2Owner(t *testing.T, scheduler *Scheduler) *slice2Owner {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	owner := &slice2Owner{cancel: cancel, done: make(chan struct{})}
	go func() {
		owner.err = scheduler.Run(ctx)
		close(owner.done)
	}()
	t.Cleanup(func() { owner.stop(t) })
	return owner
}

func (owner *slice2Owner) stop(t *testing.T) {
	t.Helper()
	owner.stopOnce.Do(func() {
		owner.cancel()
		<-owner.done
		if owner.err != nil {
			t.Errorf("scheduler stopped: %v", owner.err)
		}
	})
}

func (owner *slice2Owner) waitEntered(t *testing.T, entered <-chan struct{}) {
	t.Helper()
	select {
	case <-entered:
	case <-owner.done:
		t.Fatalf("scheduler stopped before Wait: %v", owner.err)
	case <-time.After(testTimeout(5 * time.Second)):
		t.Fatal("adopted Wait not entered")
	}
}

func (owner *slice2Owner) waitTerminal(t *testing.T, scheduler *Scheduler, database *store.Store, id string) model.Run {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), testTimeout(time.Minute))
	defer cancel()
	last := model.Run{ID: id}
	for {
		// Subscribe before the durable read: finalization between these two
		// operations must wake us, while an already terminal run needs no wait.
		changed := scheduler.signals.Run(id)
		run, err := database.Run(ctx, id)
		if err != nil {
			t.Fatalf("read terminal run %s: %v; last observed run = %#v", id, err, last)
		}
		last = run
		if run.Status.Terminal() {
			return run
		}
		select {
		case <-changed:
		case <-owner.done:
			t.Fatalf("scheduler stopped before terminal run %s: %v; last observed run = %#v", id, owner.err, last)
		case <-ctx.Done():
			t.Fatalf("waiting for terminal run %s: %v; last observed run = %#v", id, ctx.Err(), last)
		}
	}
}

func TestStartupAdoptsPromotionTagAndReleaseAcrossRepeatedRestarts(t *testing.T) {
	for _, kind := range []string{"promotion", "tag", "release"} {
		for _, finishedWhileDown := range []bool{false, true} {
			t.Run(kind+map[bool]string{false: "/mid-run", true: "/finished-while-down"}[finishedWhileDown], func(t *testing.T) {
				fixture, run, promotion := slice2RecoveryRun(t, kind)
				paths := slice2Workspaces(t, fixture, run, promotion)
				git := &controlGit{}
				git.setRemoteRefLocked("refs/heads/main", adoptionBaseSHA)
				jobs := &adoptingJobs{found: true, recoveryJobs: recoveryJobs{terminalErr: ErrJobNotTerminal, waitBlocks: make(chan struct{}), waitEntered: make(chan struct{}), waitResult: JobResult{Status: model.RunPassed, Phase: "passed", Steps: []model.StepResult{stepResult(model.StepPassed)}}}}
				// First new owner resumes live work, then shuts down again. A second
				// owner either finds it live or observes completion during the outage.
				reopenSlice2Owner(t, fixture)
				scheduler := slice2Scheduler(t, fixture, jobs, git)
				owner := startSlice2Owner(t, scheduler)
				owner.waitEntered(t, jobs.waitEntered)
				active, err := fixture.store.Run(context.Background(), run.ID)
				if err != nil || active.Status != model.RunRunning || active.Credentialed != run.Credentialed || active.Release != run.Release || active.SHA != run.SHA || active.TestedSHA != run.TestedSHA || active.AdmissionSequence != run.AdmissionSequence || active.SupersededBy != "" {
					t.Fatalf("adoption changed durable attempt: %+v %v", active, err)
				}
				owner.stop(t)
				for _, path := range paths {
					if _, err := os.Stat(path); err != nil {
						t.Fatal("active workspace removed", err)
					}
				}
				if len(jobs.deleted) != 0 || jobs.created != 0 || jobs.releaseCreated != 0 {
					t.Fatal("restart replayed or deleted work")
				}
				if head, _, _ := git.RemoteRef(context.Background(), fixture.repo.Name, "refs/heads/main"); head != adoptionBaseSHA {
					t.Fatal("published before completion")
				}
				reopenSlice2Owner(t, fixture)
				jobs = &adoptingJobs{found: true, recoveryJobs: recoveryJobs{terminalErr: ErrJobNotTerminal, waitBlocks: make(chan struct{}), waitEntered: make(chan struct{}), waitResult: JobResult{Status: model.RunPassed, Phase: "passed", Steps: []model.StepResult{stepResult(model.StepPassed)}}}}
				if finishedWhileDown {
					close(jobs.waitBlocks)
				}
				scheduler = slice2Scheduler(t, fixture, jobs, git)
				owner = startSlice2Owner(t, scheduler)
				owner.waitEntered(t, jobs.waitEntered)
				if !finishedWhileDown {
					close(jobs.waitBlocks)
				}
				final := owner.waitTerminal(t, scheduler, fixture.store, run.ID)
				owner.stop(t)
				if final.Status != model.RunPassed {
					t.Fatalf("run = %#v, want passed", final)
				}
				// Run joins adopted workers, including terminal workspace cleanup.
				// Assert after that barrier instead of racing a filesystem poll.
				for _, path := range paths {
					if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
						t.Fatalf("terminal workspace %s retained: %v", path, err)
					}
				}
				if final.ID != run.ID || final.QueueSequence != run.QueueSequence || final.AdmissionSequence != run.AdmissionSequence || final.SupersededBy != "" || final.Credentialed != run.Credentialed {
					t.Fatalf("final attempt changed: %+v", final)
				}
				if jobs.created != 0 || jobs.releaseCreated != 0 || len(jobs.deleted) != 0 || len(jobs.adopted) != 1 {
					t.Fatal("adoption submitted work again")
				}
				if counts, err := fixture.store.CountRuns(context.Background()); err != nil || counts.Total != 1 {
					t.Fatalf("requeued release/promotion: %+v %v", counts, err)
				}
				if steps, err := fixture.store.StepResults(context.Background(), run.ID); err != nil || len(steps) != 1 {
					t.Fatalf("lost final steps: %+v %v", steps, err)
				}
				if promotion.ID != "" {
					p, err := fixture.store.Promotion(context.Background(), promotion.ID)
					if err != nil || p.Status != model.PromotionPassed {
						t.Fatalf("promotion=%+v %v", p, err)
					}
				}
				if err := scheduler.recoverPendingPublications(context.Background()); err != nil {
					t.Fatal(err)
				}
				reopenSlice2Owner(t, fixture)
				recovery := slice2Scheduler(t, fixture, jobs, git)
				if err := recovery.reconcileStrandedRunsWithGate(context.Background(), recovery.requireMutation, func(model.Run, periapsis.Size) error { return errors.New("terminal run re-adopted") }); err != nil {
					t.Fatal(err)
				}
				if err := recovery.recoverPendingPublications(context.Background()); err != nil {
					t.Fatal(err)
				}
				git.mu.Lock()
				pushes, tags := len(git.promotions), len(git.syncedTags)
				git.mu.Unlock()
				if (kind == "promotion" && (pushes != 1 || tags != 0)) || (kind != "promotion" && (pushes != 0 || tags != 1)) {
					t.Fatalf("publication repeated/lost: promotions=%d tags=%d", pushes, tags)
				}
			})
		}
	}
}

func TestAdoptedPromotionRechecksMovedMainBeforePublication(t *testing.T) {
	fixture, run, promotion := slice2RecoveryRun(t, "promotion")
	paths := slice2Workspaces(t, fixture, run, promotion)
	git := &controlGit{}
	git.setRemoteRefLocked("refs/heads/main", adoptionBaseSHA)
	jobs := &adoptingJobs{found: true, recoveryJobs: recoveryJobs{terminalErr: ErrJobNotTerminal, waitBlocks: make(chan struct{}), waitEntered: make(chan struct{}), waitResult: JobResult{Status: model.RunPassed}}}
	reopenSlice2Owner(t, fixture)
	scheduler := slice2Scheduler(t, fixture, jobs, git)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- scheduler.Run(ctx) }()
	waitSlice2Worker(t, done, jobs.waitEntered)
	moved := strings.Repeat("d", 40)
	git.mu.Lock()
	git.setRemoteRefLocked("refs/heads/main", moved)
	git.mu.Unlock()
	close(jobs.waitBlocks)
	failed := waitAdoptedStatus(t, fixture.store, run.ID, model.RunFailed)
	waitSlice2Cleanup(t, paths)
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	git.mu.Lock()
	pushes := len(git.promotions)
	git.mu.Unlock()
	p, err := fixture.store.Promotion(context.Background(), promotion.ID)
	if err != nil || p.Status != model.PromotionFailed || pushes != 0 || !strings.Contains(failed.Error, "moved") {
		t.Fatalf("stale promotion pushed: run=%+v promotion=%+v pushes=%d err=%v", failed, p, pushes, err)
	}
	head, exists, err := git.RemoteRef(context.Background(), fixture.repo.Name, "refs/heads/main")
	if err != nil || !exists || head != moved {
		t.Fatal("moved main overwritten")
	}
}

func TestAdoptedTriggerPublicationRecoversPushBeforeLedgerFinalization(t *testing.T) {
	for _, kind := range []string{"promotion", "tag", "release"} {
		t.Run(kind, func(t *testing.T) {
			fixture, run, promotion := slice2RecoveryRun(t, kind)
			paths := slice2Workspaces(t, fixture, run, promotion)
			git := &controlGit{}
			git.setRemoteRefLocked("refs/heads/main", adoptionBaseSHA)
			jobs := &adoptingJobs{found: true, recoveryJobs: recoveryJobs{terminalErr: ErrJobNotTerminal, waitResult: JobResult{Status: model.RunPassed}}}
			reopenSlice2Owner(t, fixture)
			scheduler := slice2Scheduler(t, fixture, jobs, git)
			scheduler.store = &finalizeFailStore{Store: fixture.store, failNext: true}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			go func() { done <- scheduler.Run(ctx) }()
			select {
			case err := <-done:
				if err == nil || !strings.Contains(err.Error(), "injected crash") {
					t.Fatal("expected post-push crash", err)
				}
			case <-time.After(testTimeout(5 * time.Second)):
				t.Fatal("post-push failure not returned")
			}
			pending, err := fixture.store.PendingPublications(context.Background())
			if err != nil || len(pending) != 1 || pending[0].RunID != run.ID {
				t.Fatalf("publication not durable: %+v %v", pending, err)
			}
			publication := pending[0]
			for _, path := range paths {
				if _, err := os.Stat(path); err != nil {
					t.Fatal("publication workspace removed before finalization", err)
				}
			}
			git.mu.Lock()
			before := len(git.promotions) + len(git.syncedTags)
			git.mu.Unlock()
			if before != 1 {
				t.Fatal("initial publication did not push exactly once", before)
			}
			reopenSlice2Owner(t, fixture)
			// The publication ledger owns this attempt. It must never be adopted or
			// sent through pod-side release publication a second time.
			recoveredJobs := &adoptingJobs{adoptErr: errors.New("publication-owned Workflow adopted")}
			recovery := slice2Scheduler(t, fixture, recoveredJobs, git)
			ready := make(chan struct{})
			recovery.onStartupRecovered = func() { close(ready) }
			ctx2, cancel2 := context.WithCancel(context.Background())
			defer cancel2()
			done2 := make(chan error, 1)
			go func() { done2 <- recovery.Run(ctx2) }()
			select {
			case <-ready:
			case err := <-done2:
				t.Fatal("publication recovery failed", err)
			case <-time.After(testTimeout(5 * time.Second)):
				t.Fatal("publication recovery stalled")
			}
			cancel2()
			if err := <-done2; err != nil {
				t.Fatal(err)
			}
			final, err := fixture.store.Run(context.Background(), run.ID)
			if err != nil || final.Status != model.RunPassed || final.ID != run.ID {
				t.Fatalf("final=%+v %v", final, err)
			}
			durable, err := fixture.store.Publication(context.Background(), publication.ID)
			if err != nil || durable.Status != model.PublicationDelivered {
				t.Fatalf("ledger=%+v %v", durable, err)
			}
			git.mu.Lock()
			after := len(git.promotions) + len(git.syncedTags)
			git.mu.Unlock()
			if after != before || len(recoveredJobs.adopted) != 0 || recoveredJobs.created != 0 || recoveredJobs.releaseCreated != 0 || len(recoveredJobs.deleted) != 0 {
				t.Fatal("restart repeated a completed publication")
			}
			if kind != "promotion" {
				head, exists, err := git.RemoteRef(context.Background(), fixture.repo.Name, "refs/tags/"+run.Ref)
				if err != nil || !exists || head != adoptionTagObjectSHA || head == run.TestedSHA {
					t.Fatal("tag object was rebound to peeled commit")
				}
			}
			waitSlice2Cleanup(t, paths)
		})
	}
}

func TestMissingPromotionTagAndReleaseStayIneligibleForRefire(t *testing.T) {
	for _, kind := range []string{"promotion", "tag", "release"} {
		t.Run(kind, func(t *testing.T) {
			fixture, run, promotion := slice2RecoveryRun(t, kind)
			reopenSlice2Owner(t, fixture)
			jobs := &adoptingJobs{found: false, recoveryJobs: recoveryJobs{terminalErr: ErrJobNotTerminal}}
			scheduler := slice2Scheduler(t, fixture, jobs, &controlGit{})
			if err := scheduler.reconcileStrandedRunsWithGate(context.Background(), scheduler.requireMutation, func(model.Run, periapsis.Size) error { return errors.New("missing Workflow adopted") }); err != nil {
				t.Fatal(err)
			}
			final, err := fixture.store.Run(context.Background(), run.ID)
			if err != nil || final.Status != model.RunInterrupted || final.SupersededBy != "" || final.ID != run.ID || len(jobs.adopted) != 1 || len(jobs.deleted) != 1 || jobs.created != 0 || jobs.releaseCreated != 0 {
				t.Fatalf("missing fallback: %+v %v", final, err)
			}
			if counts, err := fixture.store.CountRuns(context.Background()); err != nil || counts.Total != 1 {
				t.Fatalf("unsafe re-fire: %+v %v", counts, err)
			}
			if promotion.ID != "" {
				p, err := fixture.store.Promotion(context.Background(), promotion.ID)
				if err != nil || p.Status != model.PromotionInterrupted {
					t.Fatalf("promotion=%+v %v", p, err)
				}
			}
		})
	}
}

func TestNewPromotionTagAndReleaseKeepWorkflowAndWorkspaceOnShutdown(t *testing.T) {
	for _, kind := range []string{"promotion", "tag", "release"} {
		t.Run(kind, func(t *testing.T) {
			fixture, run, promotion := slice2ClaimedRun(t, kind)
			paths := slice2Workspaces(t, fixture, run, promotion)
			jobs := &adoptingJobs{recoveryJobs: recoveryJobs{terminalErr: ErrJobNotTerminal, waitBlocks: make(chan struct{}), waitEntered: make(chan struct{})}}
			scheduler := slice2Scheduler(t, fixture, jobs, &controlGit{})
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			// Exercise the newly created worker tail (not only adopted shutdown).
			go func() { done <- scheduler.executeAndCleanup(ctx, run) }()
			waitSlice2Worker(t, done, jobs.waitEntered)
			cancel()
			if err := <-done; err != nil {
				t.Fatal(err)
			}
			active, err := fixture.store.Run(context.Background(), run.ID)
			if err != nil || active.Status != model.RunRunning || active.SupersededBy != "" || len(jobs.deleted) != 0 {
				t.Fatalf("shutdown disrupted run: %+v %v", active, err)
			}
			expectedCI, expectedRelease := 1, 0
			if run.Release {
				expectedCI, expectedRelease = 0, 1
			}
			if jobs.created != expectedCI || jobs.releaseCreated != expectedRelease {
				t.Fatal("wrong creation capability")
			}
			for _, path := range paths {
				if _, err := os.Stat(path); err != nil {
					t.Fatal("active workspace removed", err)
				}
			}
		})
	}
}

func TestLegacyNonReleaseTagKeepsFallbackWithoutInferringRelease(t *testing.T) {
	fixture := newRecoveryFixture(t)
	ctx := context.Background()
	enqueued, err := fixture.store.EnqueueRun(ctx, model.RunSpec{RepoID: fixture.repo.ID, RefKind: model.RefTag, Ref: "v1.0.0", SHA: adoptionTagObjectSHA, TestedSHA: adoptionTestedSHA, Actor: "agent@host", Trigger: "tag"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = fixture.store.ClaimNextRun(ctx); err != nil {
		t.Fatal(err)
	}
	fixture.setJobName(t, enqueued.ID, "legacy-tag-job")
	jobs := &adoptingJobs{found: true, recoveryJobs: recoveryJobs{terminalErr: ErrJobNotTerminal}}
	scheduler := fixture.scheduler(t, jobs)
	if err = scheduler.reconcileStrandedRunsWithGate(ctx, scheduler.requireMutation, func(model.Run, periapsis.Size) error { return errors.New("legacy tag adopted") }); err != nil {
		t.Fatal(err)
	}
	final, err := fixture.store.Run(ctx, enqueued.ID)
	if err != nil || final.Release || final.Status != model.RunInterrupted || final.SupersededBy != "" || len(jobs.adopted) != 0 || len(jobs.deleted) != 1 {
		t.Fatalf("legacy tag rebound: %+v %v", final, err)
	}
}
