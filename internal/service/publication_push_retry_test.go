package service

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/api"
	"github.com/oberthci/oberth/internal/gitcache"
	"github.com/oberthci/oberth/internal/gitssh"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/pushretry"
	"github.com/oberthci/oberth/internal/storetest"
)

// Use the durable scheduler path for each attempt, as both live dispatch and
// startup recovery do. Moving the persisted due date avoids wall-clock sleeps.
func TestTransientPublicationRetriesBoundedAndProtected(t *testing.T) {
	for _, ref := range []string{"feature/retry", "main", "v1.0"} {
		t.Run(ref, func(t *testing.T) {
			f := newControlFixture(t)
			kind := model.RefBranch
			if ref == "v1.0" {
				kind = model.RefTag
			}
			run, err := storetest.EnqueueRun(f.store, t.Context(), model.RunSpec{RepoID: f.repo.ID, RefKind: kind, Ref: ref, SHA: retrySourceSHA, Actor: "agent@host", Trigger: "branch"})
			if err != nil {
				t.Fatal(err)
			}
			if _, err = f.store.ClaimNextRun(t.Context()); err != nil {
				t.Fatal(err)
			}
			pub, err := f.store.BeginPublication(t.Context(), model.PublicationSpec{RepoID: f.repo.ID, RunID: run.ID, RefKind: kind, Ref: ref, ResultSHA: run.SHA, Actor: run.Actor})
			if err != nil {
				t.Fatal(err)
			}
			g := &retryPublicationGit{controlGit: f.git, failure: errors.New("remote: Forgejo: Internal Server Connection Error\npre-receive hook declined")}
			for attempt := 1; attempt <= 5; attempt++ {
				final, err := deliverPublication(t.Context(), f.store, g, f.store, f.repo, f.repo.Name, pub)
				if err != nil {
					t.Fatal(err)
				}
				pub = final.Publication
				if pub.Attempts != attempt {
					t.Fatalf("attempts = %d", pub.Attempts)
				}
				if attempt < 5 {
					if pub.Status != model.PublicationPending || !strings.Contains(pub.Error, "retry") || pub.RetryAfter.IsZero() {
						t.Fatalf("not queued: %+v", pub)
					}
					// Recovery before the backoff expires must not push.
					if _, err := deliverPublication(t.Context(), f.store, g, f.store, f.repo, f.repo.Name, pub); err != nil {
						t.Fatal(err)
					}
					if g.calls != attempt {
						t.Fatal("pushed before due date")
					}
					pub.RetryAfter = time.Now().Add(-time.Second)
					pub, err = f.store.UpdatePublicationAttempt(t.Context(), pub.ID, pub.Attempts, pub.Attempts, pub.RetryAfter, pub.Error)
					if err != nil {
						t.Fatal(err)
					}
				} else if pub.Status != model.PublicationFailed || final.Run.Phase != "infrastructure" || !pushretry.PublicationFailure(final.Run.Error) {
					t.Fatalf("exhausted = %+v", final)
				}
			}
			if g.calls != 5 || (ref == "main" && g.forced != 0) {
				t.Fatalf("calls=%d forced=%d", g.calls, g.forced)
			}
		})
	}
}

type retryPublicationGit struct {
	*controlGit
	calls, forced int
	failure       error
	delivered     bool
}

func (g *retryPublicationGit) push(ref, sha string) error {
	g.calls++
	if g.delivered {
		g.mu.Lock()
		g.setRemoteRefLocked(ref, sha)
		g.mu.Unlock()
	}
	return g.failure
}
func (g *retryPublicationGit) SyncBranch(_ context.Context, _, ref, sha string) error {
	g.forced++
	return g.push("refs/heads/"+ref, sha)
}
func (g *retryPublicationGit) SyncTag(_ context.Context, _, ref, sha string) error {
	return g.push("refs/tags/"+ref, sha)
}
func (g *retryPublicationGit) PushPromotion(_ context.Context, _, ref, sha string) error {
	return g.push("refs/heads/"+ref, sha)
}

func TestPublicationLostReplyAndPermanentRejection(t *testing.T) {
	for _, lost := range []bool{false, true} {
		t.Run(map[bool]string{false: "permission", true: "lost reply"}[lost], func(t *testing.T) {
			f := newControlFixture(t)
			run, err := storetest.EnqueueRun(f.store, t.Context(), model.RunSpec{RepoID: f.repo.ID, RefKind: model.RefBranch, Ref: "main", SHA: retrySourceSHA, Actor: "agent@host", Trigger: "branch"})
			if err != nil {
				t.Fatal(err)
			}
			if _, err = f.store.ClaimNextRun(t.Context()); err != nil {
				t.Fatal(err)
			}
			pub, err := f.store.BeginPublication(t.Context(), model.PublicationSpec{RepoID: f.repo.ID, RunID: run.ID, RefKind: run.RefKind, Ref: run.Ref, ResultSHA: run.SHA, Actor: run.Actor})
			if err != nil {
				t.Fatal(err)
			}
			failure := errors.New("permission denied")
			if lost {
				failure = errors.New("connection reset by peer")
			}
			g := &retryPublicationGit{controlGit: f.git, failure: failure, delivered: lost}
			final, err := deliverPublication(t.Context(), f.store, g, f.store, f.repo, f.repo.Name, pub)
			if err != nil {
				t.Fatal(err)
			}
			if lost && final.Publication.Status != model.PublicationDelivered {
				t.Fatalf("lost reply = %+v", final)
			}
			if !lost && (final.Publication.Status != model.PublicationFailed || final.Run.Phase != "publishing") {
				t.Fatalf("permanent = %+v", final)
			}
			if g.calls != 1 || g.forced != 0 {
				t.Fatalf("calls=%d forced=%d", g.calls, g.forced)
			}
		})
	}
}

func TestPromoteRetryReusesExactTransientPushCandidate(t *testing.T) {
	for _, moved := range []bool{false, true} {
		t.Run(map[bool]string{false: "unchanged", true: "moved"}[moved], func(t *testing.T) {
			g := &scriptedPromotionGit{plans: []gitcache.MergeCandidate{{BaseSHA: retryBaseSHA, MergedSHA: retryMergedSHA}, {BaseSHA: retryMovedSHA, MergedSHA: retryMerged2}}}
			f, s, control := newPromotionRetryHarness(t, g, passedPromotionCI, passedPromotionCI)
			actor := api.Actor{Identity: "admin@host", Admin: true}
			id := promoteForRetry(t, control, actor)
			// Simulate an exhausted push-only failure after the recorded CI passed.
			pending, err := s.store.ClaimNextRun(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			step := stepResult(model.StepPassed)
			step.RunID = pending.ID
			if _, err = f.store.PutStepResult(t.Context(), step); err != nil {
				t.Fatal(err)
			}
			promotion, err := f.store.Promotion(t.Context(), id)
			if err != nil {
				t.Fatal(err)
			}
			pub, err := s.beginRunPublication(t.Context(), f.repo, pending, promotion, JobResult{})
			if err != nil {
				t.Fatal(err)
			}
			if _, err = f.store.FinalizePublication(t.Context(), pub.ID, model.PublicationFailed, pushretry.Failure("remote: Internal Server Connection Error")); err != nil {
				t.Fatal(err)
			}
			if moved {
				g.controlGit.mu.Lock()
				g.setRemoteRefLocked("refs/heads/main", retryMovedSHA)
				g.controlGit.mu.Unlock()
			}
			value, err := control.CallTool(t.Context(), actor, "promote_retry", json.RawMessage(`{"id":"`+id+`"}`))
			if err != nil {
				t.Fatal(err)
			}
			response := value.(api.PromoteRetryResponse)
			retried, err := f.store.Promotion(t.Context(), response.ID)
			if err != nil {
				t.Fatal(err)
			}
			prepared, pushed := g.counts()
			if !moved && (prepared != 1 || pushed != 1 || retried.Status != model.PromotionPassed || retried.ResultSHA != retryMergedSHA || retried.RunID != "") {
				t.Fatalf("unchanged candidate re-tested or replaced: prepared=%d pushed=%d promotion=%+v", prepared, pushed, retried)
			}
			if moved && (prepared != 2 || retried.ResultSHA != retryMerged2 || retried.RunID == "") {
				t.Fatalf("moved target reused stale tests: prepared=%d promotion=%+v", prepared, retried)
			}
		})
	}
}

type publicationDeadlineContext struct {
	context.Context
	expired atomic.Bool
	done    chan struct{}
}

func (ctx *publicationDeadlineContext) Done() <-chan struct{} { return ctx.done }
func (ctx *publicationDeadlineContext) Err() error {
	if ctx.expired.Load() {
		return context.DeadlineExceeded
	}
	return nil
}
func (ctx *publicationDeadlineContext) expire() { ctx.expired.Store(true); close(ctx.done) }

type timedOutPublicationGit struct {
	*retryPublicationGit
	expire func()
}

func (g *timedOutPublicationGit) PushPromotion(ctx context.Context, _, _, _ string) error {
	g.calls++
	g.expire()
	return ctx.Err()
}
func TestPublicationDeadlineUsesFreshFinalizationContext(t *testing.T) {
	f := newControlFixture(t)
	_, pub := beginPendingPromotionPublication(t, f, "main", retryBaseSHA, retrySourceSHA)
	f.git.setRemoteRefLocked("refs/heads/main", retryBaseSHA)
	ctx := &publicationDeadlineContext{Context: t.Context(), done: make(chan struct{})}
	g := &timedOutPublicationGit{retryPublicationGit: &retryPublicationGit{controlGit: f.git}, expire: ctx.expire}
	final, err := deliverPublication(ctx, f.store, g, f.store, f.repo, f.repo.Name, pub)
	if err != nil || final.Publication.Status != model.PublicationPending || !strings.Contains(final.Publication.Error, "queued") {
		t.Fatalf("expired delivery did not queue with fresh context: %+v %v", final, err)
	}
	if g.calls != 1 {
		t.Fatal("push replayed")
	}
}

func TestPublishRetryQueuedResponseAndRunRetryDoNotRepeatCI(t *testing.T) {
	for _, tool := range []string{"publish_retry", "run_retry", "run_rerun"} {
		t.Run(tool, func(t *testing.T) {
			f := newControlFixture(t)
			run, err := storetest.EnqueueRun(f.store, t.Context(), model.RunSpec{RepoID: f.repo.ID, RefKind: model.RefBranch, Ref: "feature/retry", SHA: retrySourceSHA, Actor: "agent@host", Trigger: "branch"})
			if err != nil {
				t.Fatal(err)
			}
			if _, err = f.store.ClaimNextRun(t.Context()); err != nil {
				t.Fatal(err)
			}
			pub, err := f.store.BeginPublication(t.Context(), model.PublicationSpec{RepoID: f.repo.ID, RunID: run.ID, RefKind: run.RefKind, Ref: run.Ref, ResultSHA: run.SHA, Actor: run.Actor})
			if err != nil {
				t.Fatal(err)
			}
			if _, err = f.store.FinalizePublication(t.Context(), pub.ID, model.PublicationFailed, pushretry.Failure("remote: Internal Server Connection Error")); err != nil {
				t.Fatal(err)
			}
			f.scheduler.git = &retryPublicationGit{controlGit: f.git, failure: errors.New("remote: Internal Server Connection Error\npre-receive hook declined")}
			control := f.api(t)
			control.runRetries = f.store
			control.runReruns = f.store
			value, err := control.CallTool(t.Context(), api.Actor{Identity: "admin@host", Admin: true}, tool, json.RawMessage(`{"id":"`+run.ID+`"}`))
			if err != nil {
				t.Fatal(err)
			}
			if tool == "publish_retry" {
				response := value.(api.PublishRetryResponse)
				if response.Status != "pending" || !strings.Contains(response.Error, "queued") || strings.Contains(response.Error, "begin publication finalization") {
					t.Fatalf("response = %+v", response)
				}
			}
			current, err := f.store.Run(t.Context(), run.ID)
			if err != nil || current.Status != model.RunRunning || current.Phase != "publishing" {
				t.Fatalf("run = %+v %v", current, err)
			}
			if len(f.jobs.createdCI) != 0 {
				t.Fatal("CI repeated")
			}
		})
	}
}

func exhaustPublicationRetries(t *testing.T, f *controlFixture) {
	t.Helper()
	for i := 1; i < pushretry.MaxAttempts; i++ {
		pending, err := f.store.PendingPublications(t.Context())
		if err != nil || len(pending) != 1 {
			t.Fatalf("pending: %+v %v", pending, err)
		}
		pub := pending[0]
		if _, err = f.store.UpdatePublicationAttempt(t.Context(), pub.ID, pub.Attempts, pub.Attempts, time.Now().Add(-time.Second), pub.Error); err != nil {
			t.Fatal(err)
		}
		if _, err = f.scheduler.DeliverPublication(t.Context(), pub.ID); err != nil {
			t.Fatal(err)
		}
	}
}

func TestSchedulerAutomaticallyResumesDuePublication(t *testing.T) {
	f := newControlFixture(t)
	run, err := storetest.EnqueueRun(f.store, t.Context(), model.RunSpec{RepoID: f.repo.ID, RefKind: model.RefBranch, Ref: "feature/automatic", SHA: retrySourceSHA, Actor: "agent@host", Trigger: "branch"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.store.ClaimNextRun(t.Context()); err != nil {
		t.Fatal(err)
	}
	pub, err := f.store.BeginPublication(t.Context(), model.PublicationSpec{RepoID: f.repo.ID, RunID: run.ID, RefKind: run.RefKind, Ref: run.Ref, ResultSHA: run.SHA, Actor: run.Actor})
	if err != nil {
		t.Fatal(err)
	}
	g := &retryPublicationGit{controlGit: f.git, failure: errors.New("remote: Internal Server Error\npre-receive hook declined")}
	f.scheduler.git = g
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- f.scheduler.Run(ctx) }()
	defer func() {
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
	}()
	timer := time.NewTimer(testTimeout(35 * time.Second))
	defer timer.Stop()
	for {
		wake := f.signals.Run(run.ID)
		current, err := f.store.Publication(t.Context(), pub.ID)
		if err != nil {
			t.Fatal(err)
		}
		if current.Attempts == 1 && strings.Contains(current.Error, "queued") {
			// The permit orders test-side transport recovery before the next push.
			<-f.scheduler.deliveryPermit
			g.failure = nil
			_, err = f.store.UpdatePublicationAttempt(t.Context(), pub.ID, 1, 1, time.Now().Add(-time.Second), current.Error)
			f.scheduler.deliveryPermit <- struct{}{}
			if err != nil {
				t.Fatal(err)
			}
			break
		}
		select {
		case <-wake:
		case <-timer.C:
			t.Fatal("initial delivery did not queue")
		}
	}
	for {
		wake := f.signals.Run(run.ID)
		current, err := f.store.Run(t.Context(), run.ID)
		if err != nil {
			t.Fatal(err)
		}
		if current.Status == model.RunPassed {
			break
		}
		select {
		case <-wake:
		case <-timer.C:
			t.Fatal("due publication was not resumed automatically")
		}
	}
	<-f.scheduler.deliveryPermit
	calls := g.calls
	f.scheduler.deliveryPermit <- struct{}{}
	if calls != 2 || len(f.jobs.createdCI) != 0 {
		t.Fatalf("pushes=%d jobs=%d", calls, len(f.jobs.createdCI))
	}
}

// The incident wording: the git-ssh shim's transport line followed by Git's
// generic access-rights advice must queue a retry, not fail attempt one.
func TestGitSSHTransportFailureQueuesPublicationRetry(t *testing.T) {
	f := newControlFixture(t)
	run, err := storetest.EnqueueRun(f.store, t.Context(), model.RunSpec{RepoID: f.repo.ID, RefKind: model.RefBranch, Ref: "main", SHA: retrySourceSHA, Actor: "agent@host", Trigger: "branch"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.store.ClaimNextRun(t.Context()); err != nil {
		t.Fatal(err)
	}
	pub, err := f.store.BeginPublication(t.Context(), model.PublicationSpec{RepoID: f.repo.ID, RunID: run.ID, RefKind: run.RefKind, Ref: run.Ref, ResultSHA: run.SHA, Actor: run.Actor})
	if err != nil {
		t.Fatal(err)
	}
	failure := errors.New("git push: exit status 128: " + gitssh.TemporaryFailureMessage +
		"\nfatal: Could not read from remote repository.\n\nPlease make sure you have the correct access rights\nand the repository exists.")
	g := &retryPublicationGit{controlGit: f.git, failure: failure}
	final, err := deliverPublication(t.Context(), f.store, g, f.store, f.repo, f.repo.Name, pub)
	if err != nil {
		t.Fatal(err)
	}
	if final.Publication.Status != model.PublicationPending || final.Publication.Attempts != 1 || final.Publication.RetryAfter.IsZero() {
		t.Fatalf("transport failure was not queued for retry: %+v", final.Publication)
	}
}
