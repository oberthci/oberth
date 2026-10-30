package service

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/vmrunner"
)

type deadlinePreflight struct {
	*protectedFixture
	executed      atomic.Int64
	recovered     atomic.Int64
	failure       error
	beforeCleanup func()
}

func (p *deadlinePreflight) Execute(ctx context.Context, job JobRequest, requirement vmrunner.Requirement, output io.Writer) error {
	p.executed.Add(1)
	if err := p.protectedFixture.Execute(ctx, job, requirement, output); err == nil {
		return nil
	}
	return p.failure
}
func (p *deadlinePreflight) RecoverRun(ctx context.Context, id string) error {
	if p.recovered.Add(1) < 3 {
		return p.failure
	}
	if p.beforeCleanup != nil {
		p.beforeCleanup()
	}
	return p.store.CompleteKVMPreflightCleanup(ctx, id, "workflow")
}
func TestDatabaseDeadlineTrustedPreflightRetainsUntilCleanup(t *testing.T) {
	testDatabasePreflightRetainsUntilCleanup(t, context.DeadlineExceeded)
}
func TestDatabaseDeadlineCanceledAndDeadlineTrustedCleanup(t *testing.T) {
	testDatabasePreflightRetainsUntilCleanup(t, errors.Join(context.Canceled, context.DeadlineExceeded))
}
func TestDatabaseDeadlineBusyTrustedPreflight(t *testing.T) {
	testDatabasePreflightRetainsUntilCleanup(t, deadlineBusyError(5))
}
func testDatabasePreflightRetainsUntilCleanup(t *testing.T, failure error) {
	fixture := newControlFixture(t)
	contract := []byte("version: 2\nprofile: kvm-suite-amd64-v1\nsuite_sha256: " + strings.Repeat("a", 64) + "\nguest_sha256: " + strings.Repeat("b", 64) + "\nartifacts: [build/program]\n")
	fixture.scheduler.git = &policyTestGit{controlGit: fixture.git, candidate: contract, baseline: contract}
	executor := &deadlinePreflight{protectedFixture: &protectedFixture{store: fixture.store, omitCleanup: true}, failure: failure}
	executor.beforeCleanup = func() {
		// This executes on the sole recovery owner, before any cleanup or parent
		// settlement, so an asynchronously completed cleanup cannot overtake it.
		state := fixture.scheduler.AdmissionState()
		if len(state.Reservations) != 1 || state.Reservations[0].State != "retained" {
			t.Error("unclean preflight lost its retained admission", state)
		}
	}
	fixture.scheduler.trusted = executor
	queued, err := fixture.scheduler.AdmitRelease(t.Context(), ReleaseRequest{EventID: "deadline-preflight", Repository: fixture.repo, Tag: "v1.0.0", ObjectSHA: strings.Repeat("d", 40), CommitSHA: strings.Repeat("c", 40), Actor: "agent@host"})
	if err != nil {
		t.Fatal(err)
	}
	done, _ := startDeadlineScheduler(t, fixture.scheduler)
	timer := time.NewTimer(testStartTimeout(15 * time.Second))
	defer timer.Stop()
	for {
		select {
		case err := <-done:
			t.Fatal("preflight DB failure stopped scheduler", err)
		case <-timer.C:
			t.Fatal("preflight recovery did not converge")
		case <-time.After(5 * time.Millisecond):
			run, err := fixture.store.Run(t.Context(), queued.ID)
			if err != nil {
				t.Fatal(err)
			}
			if run.Status.Terminal() {
				if run.Status != model.RunInterrupted || executor.executed.Load() != 1 || executor.recovered.Load() != 3 || fixture.scheduler.DatabaseRetries() < 3 {
					t.Fatalf("unsafe preflight replay/result: run=%+v execute=%d recover=%d retries=%d", run, executor.executed.Load(), executor.recovered.Load(), fixture.scheduler.DatabaseRetries())
				}
				state, err := fixture.store.KVMPreflight(t.Context(), queued.ID)
				if err != nil || !state.Cleaned {
					t.Fatal("parent settled before cleanup", state, err)
				}
				return
			}
		}
	}
}
