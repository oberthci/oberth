package service

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/gitcache"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/pushretry"
	"github.com/oberthci/oberth/internal/store"
	"github.com/oberthci/oberth/internal/storetest"
	"github.com/oberthci/oberth/internal/workflowpolicy"
)

// Shift only the publication's age at the scheduler boundary. All durable
// state, policy checks, attempts and delivery still use the real store.
type agedPublicationStore struct {
	SchedulerStore
	age time.Duration
}

func (s *agedPublicationStore) Publication(ctx context.Context, id string) (model.Publication, error) {
	p, err := s.SchedulerStore.Publication(ctx, id)
	p.CreatedAt = p.CreatedAt.Add(-s.age)
	return p, err
}

type retryingPolicyGit struct {
	*policyTestGit
	retry *retryPublicationGit
}

func (g *retryingPolicyGit) SyncBranch(ctx context.Context, repo, ref, sha string) error {
	return g.retry.SyncBranch(ctx, repo, ref, sha)
}

func pendingPolicyRun(t *testing.T, f *controlFixture) model.Publication {
	t.Helper()
	run, err := storetest.EnqueueRun(f.store, t.Context(), model.RunSpec{RepoID: f.repo.ID, RefKind: model.RefBranch, Ref: "feature/policy-deadline", SHA: retrySourceSHA, Actor: "agent", Trigger: "branch"})
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
	return pub
}

func TestHealthyPublicationAfterDowntimeAndFifthPush(t *testing.T) {
	for _, fifth := range []bool{false, true} {
		t.Run(map[bool]string{false: "pending run after downtime", true: "fifth push after thirty minutes"}[fifth], func(t *testing.T) {
			f := newControlFixture(t)
			pub := pendingPolicyRun(t, f)
			aged := &agedPublicationStore{SchedulerStore: f.store}
			f.scheduler.store = aged
			g := &retryingPolicyGit{policyTestGit: &policyTestGit{controlGit: f.git}, retry: &retryPublicationGit{controlGit: f.git}}
			f.scheduler.git = g
			if fifth {
				g.retry.failure = errors.New("remote: Internal Server Connection Error")
				for attempt := 1; attempt < pushretry.MaxAttempts; attempt++ {
					result, err := f.scheduler.DeliverPublication(t.Context(), pub.ID)
					if err != nil {
						t.Fatal(err)
					}
					pub = result.Publication
					if pub.Status != model.PublicationPending || pub.Attempts != attempt {
						t.Fatalf("retry %d: %+v", attempt, result)
					}
					aged.age += pushretry.Delay(attempt)
					if _, err := f.store.UpdatePublicationAttempt(t.Context(), pub.ID, pub.Attempts, pub.Attempts, time.Now().Add(-time.Second), pub.Error); err != nil {
						t.Fatal(err)
					}
				}
				g.retry.failure = nil
			} else {
				aged.age = 31 * time.Minute
			}
			if aged.age < 30*time.Minute {
				t.Fatalf("probe did not cross thirty minutes: %s", aged.age)
			}
			result, err := f.scheduler.DeliverPublication(t.Context(), pub.ID)
			if err != nil {
				t.Fatal(err)
			}
			wantAttempts := 1
			if fifth {
				wantAttempts = 5
			}
			if result.Publication.Status != model.PublicationDelivered || result.Run.Status != model.RunPassed || result.Publication.Attempts != wantAttempts || g.retry.calls != wantAttempts {
				t.Fatalf("healthy old publication failed: %+v pushes=%d", result, g.retry.calls)
			}
		})
	}
}

func TestPublicationPolicyWaitPersistsAcrossRestart(t *testing.T) {
	f := newControlFixture(t)
	pub := pendingPolicyRun(t, f)
	g := &policyTestGit{controlGit: f.git, baselineErr: fmt.Errorf("%w: connection reset", gitcache.ErrPolicyUpstreamUnavailable)}
	f.scheduler.git = g
	f.scheduler.policyWaitTimeout = time.Second
	result, err := f.scheduler.DeliverPublication(t.Context(), pub.ID)
	if err != nil || result.Publication.Status != model.PublicationPending || result.Publication.Attempts != 0 {
		t.Fatalf("first unavailable: %+v %v", result, err)
	}
	if err := f.store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := storetest.Open(t.Context(), filepath.Join(f.root, "oberth.sqlite"), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	restarted, err := NewScheduler(SchedulerConfig{Store: reopened, Git: g, Logs: f.logs, Jobs: f.jobs, ReleaseJobs: f.jobs, Auditor: reopened, Signals: NewSignals(), WorkspaceRoot: filepath.Join(f.root, "restarted-work")})
	if err != nil {
		t.Fatal(err)
	}
	restarted.policyWaitTimeout = time.Second
	time.Sleep(1100 * time.Millisecond)
	result, err = restarted.DeliverPublication(t.Context(), pub.ID)
	if err != nil || result.Publication.Status != model.PublicationFailed || !strings.Contains(result.Publication.Error, "policy refresh deadline exceeded") || !strings.Contains(result.Publication.Error, "connection reset") || result.Publication.Attempts != 0 || len(f.git.syncedBranches) != 0 {
		t.Fatalf("restart reset policy waiting or consumed a push: %+v %v", result, err)
	}
}

func TestPublicationHealthyPolicyClearsExpiredWait(t *testing.T) {
	f := newControlFixture(t)
	pub := pendingPolicyRun(t, f)
	g := &retryingPolicyGit{policyTestGit: &policyTestGit{controlGit: f.git, baselineErr: fmt.Errorf("%w: connection reset", gitcache.ErrPolicyUpstreamUnavailable)}, retry: &retryPublicationGit{controlGit: f.git, failure: errors.New("remote: Internal Server Connection Error")}}
	f.scheduler.git = g
	f.scheduler.policyWaitTimeout = time.Second
	result, err := f.scheduler.DeliverPublication(t.Context(), pub.ID)
	if err != nil || result.Publication.Status != model.PublicationPending {
		t.Fatalf("first unavailable: %+v %v", result, err)
	}
	time.Sleep(1100 * time.Millisecond)
	g.baselineErr = nil
	result, err = f.scheduler.DeliverPublication(t.Context(), pub.ID)
	if err != nil || result.Publication.Status != model.PublicationPending || result.Publication.Attempts != 1 {
		t.Fatalf("healthy policy failed instead of pushing: %+v %v", result, err)
	}
	pub = result.Publication
	if _, err := f.store.UpdatePublicationAttempt(t.Context(), pub.ID, pub.Attempts, pub.Attempts, time.Now().Add(-time.Second), pub.Error); err != nil {
		t.Fatal(err)
	}
	g.baselineErr = fmt.Errorf("%w: second outage", gitcache.ErrPolicyUpstreamUnavailable)
	result, err = f.scheduler.DeliverPublication(t.Context(), pub.ID)
	if err != nil || result.Publication.Status != model.PublicationPending || result.Publication.Attempts != 1 {
		t.Fatalf("success did not reset waiting origin: %+v %v", result, err)
	}
}

type policyTestGit struct {
	*controlGit
	candidate, baseline       []byte
	candidateErr, baselineErr error
	reads                     []string
}

func (git *policyTestGit) ReadOptionalBlob(_ context.Context, repo, sha, file string, limit int) ([]byte, bool, error) {
	git.reads = append(git.reads, repo+":"+sha+":"+file)
	if file != workflowpolicy.ContractFile || limit != workflowpolicy.MaxContractBytes {
		return nil, false, errors.New("unexpected policy read")
	}
	return git.candidate, git.candidate != nil, git.candidateErr
}
func (git *policyTestGit) ReadDefaultBlob(_ context.Context, repo, file string, limit int) (string, []byte, bool, error) {
	git.reads = append(git.reads, repo+":upstream:"+file)
	if file != workflowpolicy.ContractFile || limit != workflowpolicy.MaxContractBytes {
		return "", nil, false, errors.New("unexpected policy read")
	}
	return strings.Repeat("b", 40), git.baseline, git.baseline != nil, git.baselineErr
}
