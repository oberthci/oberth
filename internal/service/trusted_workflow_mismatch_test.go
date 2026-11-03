package service

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/vmrunner"
)

type mismatchedTrustedPreflight struct {
	*protectedFixture
	ready        chan struct{}
	once         sync.Once
	allowCleanup atomic.Bool
	executed     atomic.Int64
}

func (p *mismatchedTrustedPreflight) Execute(ctx context.Context, job WorkflowRequest, requirement vmrunner.Requirement, out io.Writer) error {
	p.executed.Add(1)
	if err := p.protectedFixture.Execute(ctx, job, requirement, out); err != nil && err.Error() != "cleanup uncertain" {
		return err
	}
	if err := p.store.RecordKVMRefusal(ctx, job.Run.ID, vmrunner.KVMRefusal{Code: "preflight_failed", ObservedAt: time.Now().UTC()}); err != nil {
		return err
	}
	return &ownedWorkflowMismatch{}
}
func (p *mismatchedTrustedPreflight) RecoverRun(ctx context.Context, id string) error {
	p.once.Do(func() { close(p.ready) })
	if !p.allowCleanup.Load() {
		return errors.New("cleanup pending")
	}
	return p.store.CompleteKVMPreflightCleanup(ctx, id, "workflow")
}

type mismatchPolicyGit struct{ *policyTestGit }

func (g *mismatchPolicyGit) ReadOptionalBlob(ctx context.Context, repo, sha, file string, limit int) ([]byte, bool, error) {
	if strings.HasSuffix(repo, "/unrelated") {
		return nil, false, nil
	}
	return g.policyTestGit.ReadOptionalBlob(ctx, repo, sha, file, limit)
}
func (g *mismatchPolicyGit) ReadDefaultBlob(ctx context.Context, repo, file string, limit int) (string, []byte, bool, error) {
	if strings.HasSuffix(repo, "/unrelated") {
		return strings.Repeat("a", 40), nil, false, nil
	}
	return g.policyTestGit.ReadDefaultBlob(ctx, repo, file, limit)
}

func TestTrustedWorkflowMismatchRetainsRunUntilConfirmedCleanup(t *testing.T) {
	f := newControlFixture(t, JobResult{Status: model.RunFailed, Phase: "pipeline"})
	contract := []byte("version: 2\nprofile: kvm-suite-amd64-v1\nsuite_sha256: " + strings.Repeat("a", 64) + "\nguest_sha256: " + strings.Repeat("b", 64) + "\nartifacts: [build/program]\n")
	f.scheduler.git = &mismatchPolicyGit{&policyTestGit{controlGit: f.git, candidate: contract, baseline: contract}}
	executor := &mismatchedTrustedPreflight{protectedFixture: &protectedFixture{store: f.store, omitReceipt: true, omitCleanup: true}, ready: make(chan struct{})}
	f.scheduler.trusted = executor
	original, err := f.scheduler.AdmitRelease(t.Context(), ReleaseRequest{EventID: "trusted-mismatch", Repository: f.repo, Tag: "v1.0.0", ObjectSHA: strings.Repeat("d", 40), CommitSHA: strings.Repeat("c", 40), Actor: "agent@host"})
	if err != nil {
		t.Fatal(err)
	}
	done, _ := startDeadlineScheduler(t, f.scheduler)
	select {
	case <-executor.ready:
	case err := <-done:
		t.Fatalf("trusted mismatch stopped the scheduler: %v", err)
	case <-time.After(testStartTimeout(5 * time.Second)):
		t.Fatal("trusted mismatch did not enter exact-run cleanup")
	}
	state := f.scheduler.AdmissionState()
	if len(state.Reservations) != 1 || state.Reservations[0].State != "retained" {
		t.Fatalf("unclean trusted Workflow released admission: %+v", state)
	}
	current, err := f.store.Run(t.Context(), original.ID)
	if err != nil || current.Status != model.RunRunning {
		t.Fatalf("unclean parent terminalized: %+v %v", current, err)
	}
	otherRepo, err := f.store.CreateRepository(t.Context(), model.RepositorySpec{Name: "unrelated", UpstreamID: f.repo.UpstreamID, DefaultBranch: "main"})
	if err != nil {
		t.Fatal(err)
	}
	other, err := f.scheduler.EnqueueCI(t.Context(), CIRequest{EventID: "after-trusted-mismatch", Repository: otherRepo, Branch: "other", SHA: strings.Repeat("e", 40), Actor: "agent@host"})
	if err != nil {
		t.Fatal(err)
	}
	waitForFixtureRunStatus(t, f, other.ID, model.RunFailed)
	executor.allowCleanup.Store(true)
	waitForFixtureRunStatus(t, f, original.ID, model.RunFailed)
	current, err = f.store.Run(t.Context(), original.ID)
	if err != nil || !strings.Contains(current.Error, "Workflow spec mismatch") || executor.executed.Load() != 1 {
		t.Fatalf("mismatch result/replay: %+v executions=%d err=%v", current, executor.executed.Load(), err)
	}
	deadline := time.NewTimer(testStartTimeout(time.Second))
	defer deadline.Stop()
	for {
		held := false
		for _, reservation := range f.scheduler.AdmissionState().Reservations {
			if reservation.RunID == original.ID {
				held = true
			}
		}
		if !held {
			break
		}
		select {
		case <-deadline.C:
			t.Fatal("confirmed trusted cleanup did not release admission")
		case <-time.After(time.Millisecond):
		}
	}
	if !f.scheduler.Recovered() {
		t.Fatal("trusted mismatch withdrew readiness")
	}
}
