package service

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/api"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/vmrunner"
	"github.com/oberthci/oberth/pkg/periapsis"
)

type cancelPreflightFixture struct {
	*protectedFixture
	cancelled bool
}

func (p *cancelPreflightFixture) CancelPreflight(context.Context, string) error {
	p.cancelled = true
	return errors.New("pod remains terminating")
}
func TestPreflightCancelReleasesAdmissionBeforeCleanup(t *testing.T) {
	f, _, intent := kvmReadFixture(t)
	ctx := t.Context()
	run, err := f.store.Run(ctx, intent.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.ReserveKVMPreflight(ctx, intent); err != nil {
		t.Fatal(err)
	}
	if err := f.store.SubmitKVMPreflight(ctx, run.ID); err != nil {
		t.Fatal(err)
	}
	p := &cancelPreflightFixture{protectedFixture: &protectedFixture{store: f.store}}
	f.scheduler.trusted = p
	reservation := f.scheduler.admission.reserve(run.QueueSequence)
	defer acquireRequestWithinBudget(t, reservation, groupRequest(run.ID, run.RepoID, periapsis.M, ""))()
	reader := f.api(t)
	reader.runCancels = f.store
	reader.kvmPreflights = f.store
	response, err := reader.CancelRun(ctx, api.Actor{Identity: "owner", Admin: true}, run.ID, "stuck preflight", false)
	if err != nil {
		t.Fatal(err)
	}
	if response.Workflow != cancelWorkflowPending {
		t.Fatalf("stuck preflight reported as %q instead of pending", response.Workflow)
	}
	if used, runs := gateUsage(f.scheduler.admission); used != 0 || runs != 0 {
		t.Errorf("cancel held admission while pod stuck: %d/%d", used, runs)
	}
	if !p.cancelled {
		t.Error("cancel never targeted preflight")
	}
	state, err := f.store.KVMPreflight(ctx, run.ID)
	if err != nil || state.Cleaned {
		t.Fatal("cleanup obligation must remain until absence", err)
	}
}
func TestPreflightDeadlineVisibleInAPI(t *testing.T) {
	f, api, intent := kvmReadFixture(t)
	if _, err := f.store.ReserveKVMPreflight(t.Context(), intent); err != nil {
		t.Fatal(err)
	}
	if err := f.store.SubmitKVMPreflight(t.Context(), intent.RunID); err != nil {
		t.Fatal(err)
	}
	run, err := f.store.Run(t.Context(), intent.RunID)
	if err != nil {
		t.Fatal(err)
	}
	got := api.scheduling(t.Context(), run, nil)
	if got.Execution == nil || got.Execution.Deadline == nil || got.Execution.Deadline.ExpiresAt == nil || !got.Execution.Deadline.ExpiresAt.Equal(intent.Deadline) {
		t.Fatalf("API hid preflight deadline: %+v", got)
	}
}

type deadlinePreflightFixture struct{ *protectedFixture }

func (p *deadlinePreflightFixture) Execute(ctx context.Context, job JobRequest, requirement vmrunner.Requirement, output io.Writer) error {
	// Leave a submitted workflow and cleanup obligation, then never report a
	// controller result. The server deadline must still release admission.
	p.omitCleanup = true
	p.omitReceipt = true
	if err := p.protectedFixture.Execute(ctx, job, requirement, output); err == nil {
		return errors.New("expected pending cleanup")
	}
	deadline, cancel := context.WithTimeout(ctx, time.Millisecond)
	defer cancel()
	<-deadline.Done()
	return errors.Join(ErrPreflightDeadline, deadline.Err())
}
func TestPreflightDeadlineFailsRunAndReleasesAdmissionWithPodPending(t *testing.T) {
	f := newControlFixture(t)
	contract := []byte("version: 2\nprofile: kvm-suite-amd64-v1\nsuite_sha256: " + strings.Repeat("a", 64) + "\nguest_sha256: " + strings.Repeat("b", 64) + "\nartifacts: [build/program]\n")
	f.scheduler.git = &policyTestGit{controlGit: f.git, candidate: contract, baseline: contract}
	p := &deadlinePreflightFixture{protectedFixture: &protectedFixture{store: f.store}}
	f.scheduler.trusted = p
	queued, err := f.scheduler.AdmitRelease(t.Context(), ReleaseRequest{EventID: "deadline", Repository: f.repo, Tag: "v1.0.0", ObjectSHA: strings.Repeat("d", 40), CommitSHA: strings.Repeat("c", 40), Actor: "owner"})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.scheduler.ProcessNext(t.Context()); err != nil {
		t.Fatal(err)
	}
	run, err := f.store.Run(t.Context(), queued.ID)
	if err != nil || run.Status != model.RunFailed || !strings.Contains(run.Error, model.ExecutionDeadlineMessage) {
		t.Fatalf("deadline not finalized: %+v %v", run, err)
	}
	if used, runs := gateUsage(f.scheduler.admission); used != 0 || runs != 0 {
		t.Fatalf("expired preflight retains admission: %d/%d", used, runs)
	}
	state, err := f.store.KVMPreflight(t.Context(), run.ID)
	if err != nil || state.Cleaned {
		t.Fatal("pending pod lost cleanup obligation", err)
	}
	if len(f.jobs.createdReleases) != 0 {
		t.Fatal("deadline allowed main workflow submission")
	}
}
