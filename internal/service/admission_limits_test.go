package service

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/store"
	"github.com/oberthci/oberth/pkg/pipeline"
)

type stalledGrantJobs struct{ *fakeJobs }

func (j stalledGrantJobs) AwaitAdmission(ctx context.Context) error { <-ctx.Done(); return ctx.Err() }

func TestAdmissionLimitsGrantedBeforeSubmissionDeadlineReleasesReservation(t *testing.T) {
	f := newControlFixture(t)
	f.scheduler.jobs = stalledGrantJobs{f.jobs}
	f.scheduler.stalledRunTimeout = 20 * time.Millisecond
	queued, err := f.scheduler.EnqueueCI(t.Context(), CIRequest{EventID: "stalled-grant", Repository: f.repo, Branch: "stalled-grant", SHA: strings.Repeat("a", 40), Actor: "agent"})
	if err != nil {
		t.Fatal(err)
	}
	if err = f.scheduler.ProcessNext(t.Context()); err != nil {
		t.Fatal(err)
	}
	run, err := f.store.Run(t.Context(), queued.ID)
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != model.RunFailed || !strings.Contains(run.Error, "controller profile verification unavailable for") || strings.Contains(run.Error, "no scheduled pod") || len(f.jobs.createdCI) != 0 {
		t.Fatalf("stalled grant was not settled: %+v", run)
	}
	state := f.scheduler.AdmissionState()
	if state.Runs != 0 || state.NumericBudget != nil || len(state.Reservations) != 0 {
		t.Fatalf("stalled grant leaked resources: %+v", state)
	}
}

type admissionExpiredFailure struct{}

func (*admissionExpiredFailure) Error() string {
	return "executor identity readiness deadline exceeded after one issuance retry"
}
func (*admissionExpiredFailure) InfrastructureFailure() bool   { return true }
func (*admissionExpiredFailure) AdmissionExpiryReason() string { return "identity" }

type expiredCleanupJobs struct{ *pendingCreateJobs }

func (*expiredCleanupJobs) Delete(context.Context, string, string) error {
	return errors.New("API unavailable")
}

func TestAdmissionLimitsCleanupOutageKeepsServerAvailableAndEventuallyReleases(t *testing.T) {
	f := newControlFixture(t)
	f.jobs.waitErr = &admissionExpiredFailure{}
	now := time.Now()
	f.scheduler.createNow = func() time.Time { return now }
	absent := false
	jobs := &expiredCleanupJobs{&pendingCreateJobs{fakeJobs: f.jobs, settle: func(context.Context, string, string, time.Time) (bool, error) { return absent, nil }}}
	f.scheduler.jobs = jobs
	queued, err := f.scheduler.EnqueueCI(t.Context(), CIRequest{EventID: "identity-cleanup", Repository: f.repo, Branch: "identity-cleanup", SHA: strings.Repeat("a", 40), Actor: "agent"})
	if err != nil {
		t.Fatal(err)
	}
	if err = f.scheduler.ProcessNext(t.Context()); err != nil {
		t.Fatalf("cleanup outage stopped scheduler: %v", err)
	}
	run, err := f.store.Run(t.Context(), queued.ID)
	if err != nil || run.Error != store.ExecutorIdentityPendingMessage || !isPendingCreate(run) {
		t.Fatalf("deadline cleanup cause was lost: %+v %v", run, err)
	}
	if state := f.scheduler.AdmissionState(); state.Runs != 1 || state.NumericBudget != nil {
		t.Fatal("capacity released while workflow may live", state)
	}
	f.scheduler.settleOneCreate(t.Context())
	absent = true
	remove := f.scheduler.workspaces.remove
	f.scheduler.workspaces.remove = func(string) error { return errors.New("scratch cleanup unavailable") }
	now = now.Add(2 * time.Minute)
	f.scheduler.settleOneCreate(t.Context())
	run, err = f.store.Run(t.Context(), queued.ID)
	if err != nil || run.Status != model.RunFailed || !strings.Contains(run.Error, "executor identity readiness deadline") {
		t.Fatalf("cleanup failed to settle deadline: %+v %v", run, err)
	}
	if state := f.scheduler.AdmissionState(); state.Runs != 0 || state.NumericBudget != nil || f.scheduler.pendingCreateCount() != 1 {
		t.Fatalf("scratch cleanup retained physical capacity: %+v", state)
	}
	f.scheduler.workspaces.remove = remove
	now = now.Add(2 * time.Minute)
	f.scheduler.settleOneCreate(t.Context())
	if f.scheduler.pendingCreateCount() != 0 {
		t.Fatal("scratch cleanup did not eventually settle")
	}
}

func TestAdmissionLimitsUnclaimedQueueReportsFullRunCeilingWithoutLeakingScope(t *testing.T) {
	g := newWorkflowAdmissionForJobs(10)
	var releases []func()
	defer func() {
		for _, release := range releases {
			release()
		}
	}()
	for i := int64(1); i <= 10; i++ {
		releases = append(releases, acquireRequestWithinBudget(t, g.reserve(i), admissionRequest{Size: pipeline.M}))
	}
	api := &API{admission: &Scheduler{admission: g}}
	run := model.Run{ID: "queued", RepoID: 1, Status: model.RunQueued}
	observed, reason := api.observeScheduling(t.Context(), run, nil, true)
	if observed.Message != "10 of 10 runs running" || reason != observed.Message {
		t.Fatalf("unclaimed row hides real blocker: message=%q reason=%q", observed.Message, reason)
	}
	api.scope = &repoScope{repos: map[int64]model.Repository{1: {ID: 1}}}
	observed, reason = api.observeScheduling(t.Context(), run, nil, true)
	if strings.Contains(observed.Message, "10") || strings.Contains(reason, "10") {
		t.Fatal("scoped queue exposes shared run count", observed.Message, reason)
	}
}

func TestAdmissionLimitsPendingCleanupShowsDeadlineReason(t *testing.T) {
	for _, reason := range []string{store.ExecutorIdentityPendingMessage, store.AdmissionStalledPendingMessage} {
		run := model.Run{ID: "pending", Status: model.RunRunning, Phase: "infrastructure-pending", Error: reason}
		got, _ := new(API).observeScheduling(t.Context(), run, nil, true)
		if got.State != "infra-pending" || got.Message != reason {
			t.Fatalf("pending cleanup hides deadline reason: %+v", got)
		}
	}
}
