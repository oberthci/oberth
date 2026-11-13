package service

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/store"
)

type admissionExpiredFailure struct{}

func (*admissionExpiredFailure) Error() string {
	return "executor identity readiness deadline exceeded after one issuance retry"
}
func (*admissionExpiredFailure) InfrastructureFailure() bool   { return true }
func (*admissionExpiredFailure) ExecutorIdentityExpired() bool { return true }

type expiredCleanupJobs struct{ *pendingCreateJobs }

func (*expiredCleanupJobs) Stop(context.Context, string, string) error {
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
	if state := f.scheduler.submissionStateForTest(); state.Runs != 1 {
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
	if state := f.scheduler.submissionStateForTest(); state.Runs != 0 || f.scheduler.pendingCreateCount() != 1 {
		t.Fatalf("scratch cleanup retained physical capacity: %+v", state)
	}
	f.scheduler.workspaces.remove = remove
	now = now.Add(2 * time.Minute)
	f.scheduler.settleOneCreate(t.Context())
	if f.scheduler.pendingCreateCount() != 0 {
		t.Fatal("scratch cleanup did not eventually settle")
	}
}

func TestAdmissionLimitsPendingCleanupShowsDeadlineReason(t *testing.T) {
	for _, reason := range []string{store.ExecutorIdentityPendingMessage} {
		run := model.Run{ID: "pending", Status: model.RunRunning, Phase: "infrastructure-pending", Error: reason}
		got, _ := new(API).observeScheduling(t.Context(), run, nil, true)
		if got.State != "infra-pending" || got.Message != "workflow startup timed out; cleanup pending" || run.Error != reason {
			t.Fatalf("pending cleanup hides deadline reason: %+v", got)
		}
	}
}
