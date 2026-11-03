package service

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/oberthci/oberth/internal/api"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/vmrunner"
)

type lingeringPreflight struct {
	*protectedFixture
	deadline      bool
	deleteFailure bool
	started       chan struct{}
	ownerReturn   chan struct{}
	absent        atomic.Bool
	readbacks     atomic.Int64
}

func (p *lingeringPreflight) Execute(ctx context.Context, job WorkflowRequest, requirement vmrunner.Requirement, output io.Writer) error {
	if err := p.protectedFixture.Execute(ctx, job, requirement, output); err == nil {
		return errors.New("expected pending cleanup")
	}
	close(p.started)
	if p.deadline {
		return ErrPreflightDeadline
	}
	select {
	case <-p.ownerReturn:
		return context.Canceled
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (p *lingeringPreflight) CancelPreflight(context.Context, string) error {
	if p.deleteFailure {
		return errors.New("deletion unavailable")
	}
	return nil
}

func (p *lingeringPreflight) RecoverRun(ctx context.Context, id string) error {
	absent := p.absent.Load()
	p.readbacks.Add(1)
	if !absent {
		return errors.New("owned objects still present")
	}
	state, err := p.store.KVMPreflight(ctx, id)
	if err != nil {
		return err
	}
	return p.store.CompleteKVMPreflightCleanup(ctx, id, state.WorkflowUID)
}

func awaitPreflightCleanupCondition(t *testing.T, condition func() bool) {
	t.Helper()
	timer := time.NewTimer(testStartTimeout(12 * time.Second))
	defer timer.Stop()
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for !condition() {
		select {
		case <-timer.C:
			t.Fatal("preflight cleanup did not converge without a restart")
		case <-tick.C:
		}
	}
}

func TestPreflightCleanupAfterDeadlineOrCancelWithoutRestart(t *testing.T) {
	for _, mode := range []string{"deadline", "cancel", "cancel-delete-error"} {
		t.Run(mode, func(t *testing.T) {
			f := newControlFixture(t)
			contract := []byte("version: 2\nprofile: kvm-suite-amd64-v1\nsuite_sha256: " + strings.Repeat("a", 64) + "\nguest_sha256: " + strings.Repeat("b", 64) + "\nartifacts: [build/program]\n")
			f.scheduler.git = &policyTestGit{controlGit: f.git, candidate: contract, baseline: contract}
			p := &lingeringPreflight{protectedFixture: &protectedFixture{store: f.store, omitCleanup: true, omitReceipt: true}, deadline: mode == "deadline", deleteFailure: mode == "cancel-delete-error", started: make(chan struct{}), ownerReturn: make(chan struct{})}
			f.scheduler.trusted = p
			ready := make(chan struct{})
			f.scheduler.onStartupRecovered = func() { close(ready) }
			done, stop := startDeadlineScheduler(t, f.scheduler)
			select {
			case <-ready:
			case <-time.After(testStartTimeout(time.Second)):
				t.Fatal("scheduler did not start")
			}
			queued, err := f.scheduler.AdmitRelease(t.Context(), ReleaseRequest{EventID: mode, Repository: f.repo, Tag: "v1.0.0", ObjectSHA: strings.Repeat("d", 40), CommitSHA: strings.Repeat("c", 40), Actor: "owner"})
			if err != nil {
				t.Fatal(err)
			}
			select {
			case <-p.started:
			case <-time.After(testStartTimeout(time.Second)):
				t.Fatal("preflight did not start")
			}
			if !p.deadline {
				reader := f.api(t)
				reader.runCancels, reader.kvmPreflights = f.store, f.store
				response, err := reader.CancelRun(t.Context(), api.Actor{Identity: "owner", Admin: true}, queued.ID, "stuck preflight", true)
				if err != nil {
					t.Fatal(err)
				}
				want := cancelWorkflowDeleted
				if p.deleteFailure {
					want = cancelWorkflowPending
				}
				if response.Workflow != want {
					t.Fatalf("cancellation deletion status = %s, want %s", response.Workflow, want)
				}
				// A terminal parent does not mean its create/observer owner exited.
				// Let a reconciliation tick pass while that owner is still active.
				time.Sleep(5100 * time.Millisecond)
				if p.readbacks.Load() != 0 {
					t.Fatal("recovery raced a live preflight owner")
				}
				close(p.ownerReturn)
			}
			awaitPreflightCleanupCondition(t, func() bool {
				run, err := f.store.Run(t.Context(), queued.ID)
				return err == nil && run.Status.Terminal()
			})
			if used, runs := gateUsage(f.scheduler.admission); used != 0 || runs != 0 {
				t.Fatalf("terminal preflight retained admission: %d/%d", used, runs)
			}
			awaitPreflightCleanupCondition(t, func() bool { return p.readbacks.Load() > 0 })
			state, err := f.store.KVMPreflight(t.Context(), queued.ID)
			if err != nil || state.Cleaned {
				t.Fatal("lingering objects lost cleanup obligation", err)
			}
			p.absent.Store(true)
			awaitPreflightCleanupCondition(t, func() bool {
				state, err := f.store.KVMPreflight(t.Context(), queued.ID)
				return err == nil && state.Cleaned
			})
			if p.readbacks.Load() < 2 {
				t.Fatal("cleanup did not retry the failed absence readback")
			}
			stop()
			if err := <-done; err != nil {
				t.Fatal(err)
			}
			run, err := f.store.Run(t.Context(), queued.ID)
			want := model.RunInterrupted
			if p.deadline {
				want = model.RunFailed
			}
			if err != nil || run.Status != want || run.SupersededBy != "" || len(f.jobs.createdReleases) != 0 {
				t.Fatalf("cleanup changed terminal outcome or delivered release: %+v, %v", run, err)
			}
			// Capacity must be reusable in this process after confirmed absence.
			_, err = f.scheduler.AdmitRelease(t.Context(), ReleaseRequest{EventID: mode + "-next", Repository: f.repo, Tag: "v1.0.1", ObjectSHA: strings.Repeat("e", 40), CommitSHA: strings.Repeat("f", 40), Actor: "owner"})
			if err != nil {
				t.Fatal(err)
			}
			next, err := f.store.ClaimNextRun(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			intent := state.Intent
			intent.RunID, intent.CandidateSHA, intent.WorkflowName = next.ID, next.TestedSHA, "trusted-"+next.ID
			intent.AdmittedAt = time.Now().UTC()
			intent.Deadline = intent.AdmittedAt.Add(time.Minute)
			if _, err := f.store.ReserveKVMPreflight(t.Context(), intent); err != nil {
				t.Fatalf("confirmed cleanup retained VM capacity: %v", err)
			}
		})
	}
}

type terminalCleanupInventory struct {
	recoveryInventory
	run   model.Run
	reads int
}

func (s *terminalCleanupInventory) PendingKVMPreflights(context.Context) ([]vmrunner.KVMExecution, error) {
	s.reads++
	return []vmrunner.KVMExecution{{Intent: vmrunner.KVMIntent{RunID: "live"}}}, nil
}

func (s *terminalCleanupInventory) Run(context.Context, string) (model.Run, error) {
	return s.run, nil
}

func TestPreflightCleanupSkipsRunningParentsAndActiveOwners(t *testing.T) {
	for _, status := range []model.RunStatus{model.RunRunning, model.RunFailed} {
		t.Run(string(status), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				inventory := &terminalCleanupInventory{run: model.Run{ID: "live", Status: status}}
				executor := &slowRecoveryPreflight{}
				scheduler := &Scheduler{store: inventory, trusted: executor, mutationGate: func(context.Context) error { return nil }}
				if status.Terminal() {
					finish := scheduler.beginEvidenceExecution("live")
					defer finish()
				}
				ctx, cancel := context.WithTimeout(t.Context(), 11*time.Second)
				defer cancel()
				scheduler.runTerminalPreflightCleanup(ctx)
				if inventory.reads != 2 || executor.calls != 0 {
					t.Fatalf("cleanup touched an active execution: inventory reads=%d, recovery calls=%d", inventory.reads, executor.calls)
				}
			})
		})
	}
}
