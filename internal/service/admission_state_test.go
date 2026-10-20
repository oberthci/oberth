package service

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/oberthci/oberth/internal/api"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/store"
	"github.com/oberthci/oberth/pkg/periapsis"
)

type recordingAuditor struct {
	mu      sync.Mutex
	actions []model.AuditActionSpec
	next    Auditor
}

func (auditor *recordingAuditor) AppendAuditAction(ctx context.Context, spec model.AuditActionSpec) (model.AuditAction, error) {
	auditor.mu.Lock()
	auditor.actions = append(auditor.actions, spec)
	auditor.mu.Unlock()
	return auditor.next.AppendAuditAction(ctx, spec)
}

type failingDelete struct {
	*concurrentJobs
	err error
}

func (jobs failingDelete) Delete(context.Context, string, string) error { return jobs.err }

// TestRetainedReservationIsListedAndReleasable is #1099's acceptance: a
// reservation retained after its owner stopped shows in the admission state
// with its run and weight, admission_release clears it after proving the
// Workflow gone, and the run waiting behind it is admitted without a restart.
func TestRetainedReservationIsListedAndReleasable(t *testing.T) {
	fixture := newConcurrencyFixture(t, periapsis.M, 1)
	scheduler := fixture.scheduler
	auditor := &recordingAuditor{next: fixture.store}
	scheduler.auditor = auditor
	ctx := context.Background()

	stale, err := scheduler.EnqueueCI(ctx, CIRequest{EventID: "stale", Repository: fixture.repo, Branch: "feature/stale",
		SHA: strings.Repeat("a", 40), Actor: "agent@host"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.store.ClaimNextRun(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.store.SetRunJobName(ctx, stale.ID, "oberth-oberth-stale"); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.store.FinishRun(ctx, stale.ID, model.RunResult{Status: model.RunFailed, Phase: "job", Error: "lost"}); err != nil {
		t.Fatal(err)
	}
	gate := scheduler.admission
	held := gate.reserve(stale.QueueSequence)
	acquireRequestWithinBudget(t, held, admissionRequest{RunID: stale.ID, RepoID: fixture.repo.ID, Repo: fixture.repo.Name, Ref: "feature/stale", Size: periapsis.M})
	held.retain()

	waiter := gate.reserve(stale.QueueSequence + 1)
	waiterReady := make(chan func(), 1)
	go acquireRequestForTest(waiter, admissionRequest{RunID: "waiting-run", RepoID: fixture.repo.ID, Repo: fixture.repo.Name, Ref: "feature/next", Size: periapsis.M}, waiterReady)
	awaitWaiting(t, waiter)

	state := scheduler.AdmissionState()
	if state.Used != 2 || state.Capacity != 2 || len(state.Reservations) != 2 {
		t.Fatalf("state = %#v", state)
	}
	retained, waiting := state.Reservations[0], state.Reservations[1]
	if retained.RunID != stale.ID || retained.State != "retained" || retained.Weight != 2 || retained.Since.IsZero() || retained.Workflow == "" {
		t.Fatalf("retained view = %#v", retained)
	}
	if waiting.RunID != "waiting-run" || waiting.State != "waiting" || waiting.Position != 1 {
		t.Fatalf("waiting view = %#v", waiting)
	}

	// A reservation without a durable run is not released on a guess.
	if _, err := scheduler.ReleaseAdmission(ctx, "waiting-run", "admin@host", false); !errors.Is(err, ErrAdmissionReleaseRefused) {
		t.Fatalf("release of a reservation with no durable run = %v", err)
	}

	// Without proof the Workflow is gone, release is refused unless forced.
	scheduler.jobs = failingDelete{newConcurrentJobs(periapsis.M, 1), errors.New("apiserver unavailable")}
	if _, err := scheduler.ReleaseAdmission(ctx, stale.ID, "admin@host", false); !errors.Is(err, ErrAdmissionReleaseRefused) {
		t.Fatalf("unverified release = %v", err)
	}
	if len(auditor.actions) != 0 {
		t.Fatal("a refused release was audited as done")
	}
	scheduler.jobs = fixture.jobs
	released, err := scheduler.ReleaseAdmission(ctx, stale.ID, "admin@host", false)
	if err != nil || !released.Verified || released.State != "retained" || released.Weight != 2 {
		t.Fatalf("release = %#v, %v", released, err)
	}
	awaitAcquired(t, waiterReady)()
	if len(auditor.actions) != 1 || auditor.actions[0].Action != "admission.release" || auditor.actions[0].ResourceID != stale.ID ||
		!strings.Contains(auditor.actions[0].Details, `"verified":true`) {
		t.Fatalf("audit = %#v", auditor.actions)
	}
	if _, err := scheduler.ReleaseAdmission(ctx, stale.ID, "admin@host", false); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("second release = %v", err)
	}
}

func TestAdmissionReleaseRefusesLiveRunsAndNonAdmins(t *testing.T) {
	fixture := newConcurrencyFixture(t, periapsis.M, 1)
	scheduler := fixture.scheduler
	ctx := context.Background()
	live, err := scheduler.EnqueueCI(ctx, CIRequest{EventID: "live", Repository: fixture.repo, Branch: "feature/live",
		SHA: strings.Repeat("b", 40), Actor: "agent@host"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.store.ClaimNextRun(ctx); err != nil {
		t.Fatal(err)
	}
	reservation := scheduler.admission.reserve(live.QueueSequence)
	release := acquireRequestWithinBudget(t, reservation, admissionRequest{RunID: live.ID, RepoID: fixture.repo.ID, Repo: fixture.repo.Name, Ref: "feature/live", Size: periapsis.M})
	defer release()
	if _, err := scheduler.ReleaseAdmission(ctx, live.ID, "admin@host", true); !errors.Is(err, ErrAdmissionReleaseRefused) ||
		!strings.Contains(err.Error(), "run_cancel") {
		t.Fatalf("force release of a live granted run = %v", err)
	}
	control := &API{admission: scheduler}
	if _, err := control.releaseAdmission(ctx, api.Actor{Identity: "agent@host"}, live.ID, false); !errors.Is(err, ErrForbidden) {
		t.Fatalf("non-admin release = %v", err)
	}
	if state := scheduler.AdmissionState(); len(state.Reservations) != 1 || state.Reservations[0].State != "granted" {
		t.Fatalf("state after refusals = %#v", state)
	}
}
