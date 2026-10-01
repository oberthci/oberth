package service

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/api"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/pkg/periapsis"
)

type schedulingObserverFunc func(context.Context, model.Run) (*model.ExecutionObservation, error)

func (f schedulingObserverFunc) ObserveScheduling(ctx context.Context, run model.Run) (*model.ExecutionObservation, error) {
	return f(ctx, run)
}

func TestSchedulingAdmissionSnapshotsFollowLifecycle(t *testing.T) {
	gate := newWeightedAdmission(2)
	scheduler := &Scheduler{admission: gate}
	first := gate.reserve(1)
	release, err := first.acquire(t.Context(), periapsis.XL)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	second := gate.reserve(2)
	if result := scheduler.AdmissionSnapshot(2); result.State != "preparing" || result.Position != 1 {
		t.Fatalf("preparing=%#v", result)
	}
	acquired := make(chan func(), 1)
	go acquireForTest(second, periapsis.M, acquired)
	assertNotAcquired(t, acquired)
	if result := scheduler.AdmissionSnapshot(2); result.State != "waiting" || result.Position != 1 || result.Used != 2 || result.Size != "M" {
		t.Fatalf("waiting=%#v", result)
	}
	if result := scheduler.AdmissionSnapshot(1); result.State != "admitted" || result.Position != 0 {
		t.Fatalf("admitted=%#v", result)
	}
	release()
	releaseSecond := awaitAcquired(t, acquired)
	releaseSecond()
	if scheduler.AdmissionSnapshot(1) != nil || scheduler.AdmissionSnapshot(2) != nil {
		t.Fatal("released reservations retained")
	}
	third := gate.reserve(3)
	third.cancel()
	if scheduler.AdmissionSnapshot(3) != nil {
		t.Fatal("canceled reservation retained")
	}
}

func TestSchedulingProjectionSkipsTerminalAndProgressedRuns(t *testing.T) {
	service := &API{schedulingObserver: schedulingObserverFunc(func(context.Context, model.Run) (*model.ExecutionObservation, error) {
		t.Fatal("unexpected live observation")
		return nil, nil
	})}
	for _, state := range []model.RunStatus{model.RunPassed, model.RunFailed, model.RunInterrupted} {
		if result := service.scheduling(t.Context(), model.Run{Status: state}, nil); result != nil {
			t.Fatalf("terminal=%#v", result)
		}
	}
	for _, state := range []model.StepStatus{model.StepRunning, model.StepPassed, model.StepFailed, model.StepSkipped} {
		if result := service.scheduling(t.Context(), model.Run{Status: model.RunRunning}, []model.StepResult{{Status: state}}); result != nil {
			t.Fatalf("progressed=%#v", result)
		}
	}
}

func TestSchedulingProjectionStartupUnavailableAndContext(t *testing.T) {
	service := &API{}
	run := model.Run{Status: model.RunRunning}
	if result := service.scheduling(t.Context(), run, nil); result.State != "unavailable" {
		t.Fatalf("nil observer=%#v", result)
	}
	calls := 0
	service.schedulingObserver = schedulingObserverFunc(func(ctx context.Context, _ model.Run) (*model.ExecutionObservation, error) {
		calls++
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) > 2*time.Second {
			t.Fatal("missing bounded deadline")
		}
		return nil, errors.New("SECRET error")
	})
	if result := service.scheduling(t.Context(), run, nil); result.State != "startup" || calls != 0 {
		t.Fatalf("startup=%#v calls=%d", result, calls)
	}
	run.JobName = "wf"
	result := service.scheduling(t.Context(), run, []model.StepResult{{Status: model.StepQueued}, {Status: model.StepPending}})
	if result.Execution.State != "unavailable" || result.ObservedAt.IsZero() || result.AgeSeconds < 0 || calls != 1 {
		t.Fatalf("unavailable=%#v calls=%d", result, calls)
	}
	body, _ := json.Marshal(result)
	if strings.Contains(string(body), "SECRET") {
		t.Fatalf("leak=%s", body)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	service.schedulingObserver = schedulingObserverFunc(func(ctx context.Context, _ model.Run) (*model.ExecutionObservation, error) {
		if !errors.Is(ctx.Err(), context.Canceled) {
			t.Fatal("cancellation lost")
		}
		return nil, ctx.Err()
	})
	service.scheduling(ctx, run, nil)
}

func TestSchedulingAvailableInMCPStatusRunGetAndAPIRun(t *testing.T) {
	fixture := newControlFixture(t)
	run, err := fixture.scheduler.EnqueueCI(t.Context(), CIRequest{EventID: "scheduling", Repository: fixture.repo, Branch: "feature/scheduling", SHA: strings.Repeat("a", 40), Actor: "agent@host"})
	if err != nil {
		t.Fatal(err)
	}
	service := fixture.api(t)
	queued, err := service.status(t.Context(), fixture.repo.Name, run.SHA, "")
	if err != nil {
		t.Fatal(err)
	}
	if queued.Scheduling.State != "queued" || queued.Scheduling.QueuePosition != 1 {
		t.Fatalf("queued=%#v", queued.Scheduling)
	}
	running, err := fixture.store.ClaimNextRun(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	running, err = fixture.store.SetRunJobName(t.Context(), running.ID, "wf")
	if err != nil {
		t.Fatal(err)
	}
	service.schedulingObserver = schedulingObserverFunc(func(_ context.Context, got model.Run) (*model.ExecutionObservation, error) {
		if got.ID != run.ID {
			t.Fatal("wrong run")
		}
		return &model.ExecutionObservation{State: "observed", Phase: "Pending", Message: "Workflow observed."}, nil
	})
	statusValue, err := service.CallTool(t.Context(), api.Actor{Identity: "agent@host"}, "status", json.RawMessage(`{"repo":"oberth","ref":"feature/scheduling"}`))
	if err != nil {
		t.Fatal(err)
	}
	status := statusValue.(StatusResponse)
	fallback, err := service.statusFromRun(t.Context(), fixture.repo, running, "main", "")
	if err != nil {
		t.Fatal(err)
	}
	detailValue, err := service.Run(t.Context(), api.Actor{Identity: "agent@host"}, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	mcpValue, err := service.CallTool(t.Context(), api.Actor{Identity: "agent@host"}, "run_get", json.RawMessage(`{"id":"`+run.ID+`"}`))
	if err != nil {
		t.Fatal(err)
	}
	for _, observation := range []*model.RunScheduling{status.Scheduling, fallback.Scheduling, detailValue.(RunDetailResponse).Scheduling, mcpValue.(RunDetailResponse).Scheduling} {
		if observation == nil || observation.Execution.Phase != "Pending" {
			t.Fatalf("missing diagnostics: %#v", observation)
		}
	}
	statusJSON, _ := json.Marshal(status)
	detailJSON, _ := json.Marshal(detailValue)
	if !strings.Contains(string(statusJSON), `"scheduling":`) || !strings.Contains(string(detailJSON), `"Scheduling":`) || !strings.Contains(string(detailJSON), `"Run":`) {
		t.Fatal("wire shape changed")
	}
	persisted, err := fixture.store.Run(t.Context(), run.ID)
	if err != nil || !reflect.DeepEqual(persisted, running) {
		t.Fatalf("read mutated run: %#v %v", persisted, err)
	}
}

func TestWaitSchedulingAgeIncludesLongPoll(t *testing.T) {
	fixture := newControlFixture(t)
	run, err := fixture.scheduler.EnqueueCI(t.Context(), CIRequest{EventID: "scheduling-age", Repository: fixture.repo, Branch: "feature/scheduling-age", SHA: strings.Repeat("b", 40), Actor: "agent@host"})
	if err != nil {
		t.Fatal(err)
	}
	service := fixture.api(t)
	service.maximumWait = 1100 * time.Millisecond
	response, err := service.waitRun(t.Context(), fixture.repo.Name, run.SHA, "", 0, "")
	if err != nil {
		t.Fatal(err)
	}
	if !response.StillRunning || response.Scheduling == nil || response.Scheduling.AgeSeconds < 1 {
		t.Fatalf("stale observation age=%#v", response.Scheduling)
	}
}
