package service

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	clocktesting "k8s.io/utils/clock/testing"
	"k8s.io/utils/ptr"

	"github.com/oberthci/oberth/internal/api"
	"github.com/oberthci/oberth/internal/model"
)

type schedulingObserverFunc func(context.Context, model.Run) (*model.ExecutionObservation, error)

func (f schedulingObserverFunc) ObserveScheduling(ctx context.Context, run model.Run) (*model.ExecutionObservation, error) {
	return f(ctx, run)
}

func TestSchedulingProjectionSkipsTerminalAndProgressedListRows(t *testing.T) {
	service := &API{schedulingObserver: schedulingObserverFunc(func(context.Context, model.Run) (*model.ExecutionObservation, error) {
		t.Fatal("unexpected live observation")
		return nil, nil
	})}
	for _, state := range []model.RunStatus{model.RunPassed, model.RunFailed, model.RunInterrupted} {
		if result, _ := service.observeScheduling(t.Context(), model.Run{Status: state}, nil, true); result != nil {
			t.Fatalf("terminal=%#v", result)
		}
	}
	for _, state := range []model.StepStatus{model.StepRunning, model.StepPassed, model.StepFailed, model.StepSkipped} {
		if result, _ := service.observeScheduling(t.Context(), model.Run{Status: model.RunRunning}, []model.StepResult{{Status: state}}, false); result != nil {
			t.Fatalf("progressed=%#v", result)
		}
	}
}

func TestSchedulingProjectionStartupUnavailableAndContext(t *testing.T) {
	service := &API{}
	started := time.Now().UTC()
	run := model.Run{Status: model.RunRunning, StartedAt: &started}
	if result, _ := service.observeScheduling(t.Context(), run, nil, true); result.State != "unavailable" {
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
	if result, _ := service.observeScheduling(t.Context(), run, nil, true); result.State != "startup" || calls != 0 {
		t.Fatalf("startup=%#v calls=%d", result, calls)
	}
	run.WorkflowName = "wf"
	result, _ := service.observeScheduling(t.Context(), run, []model.StepResult{{Status: model.StepQueued}, {Status: model.StepPending}}, true)
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
	service.observeScheduling(ctx, run, nil, true)
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
	running, err = fixture.store.SetRunWorkflowName(t.Context(), running.ID, "wf")
	if err != nil {
		t.Fatal(err)
	}
	if running.StartedAt != nil {
		t.Fatal("claim and intent recording started an unsubmitted run")
	}
	waitingStatus, err := service.status(t.Context(), fixture.repo.Name, run.SHA, "")
	if err != nil || waitingStatus.Status != "waiting" || waitingStatus.Run.Status != model.RunWaiting || waitingStatus.Scheduling.State != "waiting" {
		t.Fatalf("unsubmitted run did not remain waiting: %#v, %v", waitingStatus, err)
	}
	listValue, err := service.Runs(t.Context(), api.Actor{Identity: "agent@host"}, api.RunFilter{Limit: 10})
	if err != nil || listValue.([]model.Run)[0].Status != model.RunWaiting {
		t.Fatalf("run list omitted waiting state: %#v, %v", listValue, err)
	}
	waitingDetail, err := service.Run(t.Context(), api.Actor{Identity: "agent@host"}, run.ID)
	if err != nil || waitingDetail.(RunDetailResponse).Run.Status != model.RunWaiting || waitingDetail.(RunDetailResponse).Run.StartedAt != nil {
		t.Fatalf("run detail started before submission: %#v, %v", waitingDetail, err)
	}
	waitingLog, err := service.RunLogLive(t.Context(), api.Actor{Identity: "agent@host"}, run.ID, 0)
	if err != nil || waitingLog.(LiveLogResponse).Status != "waiting" {
		t.Fatalf("live log started before submission: %#v, %v", waitingLog, err)
	}
	running, err = fixture.store.MarkRunSubmitted(t.Context(), running.ID)
	if err != nil || running.StartedAt == nil {
		t.Fatalf("submitted run did not get a start time: %#v, %v", running, err)
	}
	runningLog, err := service.RunLogLive(t.Context(), api.Actor{Identity: "agent@host"}, run.ID, 0)
	if err != nil || runningLog.(LiveLogResponse).Status != "running" {
		t.Fatalf("live log did not advance at submission: %#v, %v", runningLog, err)
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
	mcpValue, err := service.CallTool(t.Context(), api.Actor{Identity: "agent@host"}, "run_get", json.RawMessage(`{"id":"`+run.ID+`","summary":false}`))
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

type schedulingAgeClock struct {
	*clocktesting.FakeClock
	observed chan struct{}
}

func (c schedulingAgeClock) Since(observed time.Time) time.Duration {
	age := c.FakeClock.Since(observed)
	// Publish only after the observation age has been calculated, so the test
	// advances during the poll rather than during observation construction.
	c.observed <- struct{}{}
	return age
}

func TestWaitSchedulingAgeIncludesLongPoll(t *testing.T) {
	fixture := newControlFixture(t)
	run, err := fixture.scheduler.EnqueueCI(t.Context(), CIRequest{EventID: "scheduling-age", Repository: fixture.repo, Branch: "feature/scheduling-age", SHA: strings.Repeat("b", 40), Actor: "agent@host"})
	if err != nil {
		t.Fatal(err)
	}
	service := fixture.api(t)
	service.maximumWait = 1100 * time.Millisecond
	clock := schedulingAgeClock{clocktesting.NewFakeClock(time.Date(2026, time.October, 28, 0, 0, 0, 0, time.UTC)), make(chan struct{}, 3)}
	service.waitClock = clock
	type result struct {
		response WaitResponse
		err      error
	}
	done := make(chan result, 1)
	go func() {
		response, err := service.waitRun(t.Context(), fixture.repo.Name, run.SHA, "", 0, "")
		done <- result{response, err}
	}()
	// The initial read precedes the timer; the subscribed re-read follows it.
	// Advance only after the second observation has calculated its zero age.
	liveness := time.NewTimer(testStartTimeout(10 * time.Second))
	defer liveness.Stop()
	for range 2 {
		select {
		case <-clock.observed:
		case result := <-done:
			t.Fatalf("wait returned before final observation: %+v", result)
		case <-liveness.C:
			t.Fatal("wait did not reach final observation")
		}
	}
	clock.Step(service.maximumWait)
	var response WaitResponse
	select {
	case result := <-done:
		if result.err != nil {
			t.Fatal(result.err)
		}
		response = result.response
	case <-liveness.C:
		t.Fatal("wait did not return after fake deadline")
	}
	if !response.StillRunning || response.Scheduling == nil || response.Scheduling.AgeSeconds < 1 {
		t.Fatalf("stale observation age=%#v", response.Scheduling)
	}
}

func TestSchedulingDeadlineVisibleAfterProgressWithoutChangingDurableStatus(t *testing.T) {
	fixture := newControlFixture(t)
	ctx := t.Context()
	enqueued, err := fixture.scheduler.EnqueueCI(ctx, CIRequest{EventID: "deadline-observation", Repository: fixture.repo, Branch: "feature/deadline", SHA: strings.Repeat("a", 40), Actor: "agent@host"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = fixture.store.ClaimNextRun(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err = fixture.store.SetRunWorkflowName(ctx, enqueued.ID, "wf"); err != nil {
		t.Fatal(err)
	}
	run, err := fixture.store.MarkRunSubmitted(ctx, enqueued.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = fixture.store.PutStepResult(ctx, model.StepResult{RunID: run.ID, Stage: "test", Step: "unit", Status: model.StepPassed}); err != nil {
		t.Fatal(err)
	}
	service := fixture.api(t)
	service.schedulingObserver = schedulingObserverFunc(func(ctx context.Context, _ model.Run) (*model.ExecutionObservation, error) {
		if deadline, ok := ctx.Deadline(); !ok || time.Until(deadline) > 2*time.Second {
			t.Fatal("unbounded observation")
		}
		return &model.ExecutionObservation{State: "deadline_exceeded", Reason: "DeadlineExceeded", Phase: "Failed", Message: model.ExecutionDeadlineMessage, ReconciliationLagSeconds: ptr.To(int64(3080))}, nil
	})
	status, err := service.status(ctx, fixture.repo.Name, run.SHA, "")
	if err != nil {
		t.Fatal(err)
	}
	// The deadline is the running run's reason, never a second status.
	if status.Status != "running" || status.DisplayStatus != "running" || !strings.Contains(status.DisplayReason, model.ExecutionDeadlineMessage) || status.Scheduling.Execution.Reason != "DeadlineExceeded" {
		t.Fatalf("status = %+v", status)
	}
	value, err := service.CallTool(ctx, api.Actor{Identity: "agent@host"}, "run_get", json.RawMessage(`{"id":"`+run.ID+`","summary":false}`))
	if err != nil {
		t.Fatal(err)
	}
	detail := value.(RunDetailResponse)
	if detail.Run.DisplayStatus != "running" || !strings.Contains(detail.Run.DisplayReason, model.ExecutionDeadlineMessage) || *detail.Scheduling.Execution.ReconciliationLagSeconds != 3080 {
		t.Fatalf("detail = %+v", detail)
	}
	stored, err := fixture.store.Run(ctx, run.ID)
	if err != nil || stored.Status != model.RunRunning || stored.FinishedAt != nil {
		t.Fatalf("read mutated execution state: %+v %v", stored, err)
	}
	for _, state := range []string{"deadline_elapsed", "terminal_observed"} {
		presented := presentationRun(run)
		applyExecutionDisplay(&presented, &model.RunScheduling{Message: "Awaiting finalization.", Execution: &model.ExecutionObservation{State: state}})
		if presented.DisplayStatus != "running" {
			t.Fatal("unconfirmed expiry changed display status")
		}
	}
	terminal := run
	terminal.Status, terminal.Error = model.RunFailed, "Step exceeded its deadline"
	if got := presentationRun(terminal); got.DisplayStatus != "failed" || got.DisplayReason != "infrastructure error: deadline exceeded" {
		t.Fatalf("retained terminal deadline reason hidden: %+v", got)
	}
}

// A running run whose step is queued behind native Argo synchronization shows
// the lock in its reason line, with no new status.
func TestExecutionDisplayNamesTheSynchronizationLock(t *testing.T) {
	for name, execution := range map[string]*model.ExecutionObservation{
		"workflow-level": {State: "observed", Phase: "Pending", Message: model.LockWaitMessagePrefix + "ConfigMap/oberth-sync-limits/kvm (0/1 free)."},
		"template-level": {State: "observed", Phase: "Running", Message: "Workflow execution is running.",
			Nodes: []model.SchedulingReason{{Phase: "Succeeded"}, {Phase: "Pending", Message: model.LockWaitMessagePrefix + "ConfigMap/oberth-sync-limits/kvm (0/1 free)."}}},
	} {
		t.Run(name, func(t *testing.T) {
			run := model.Run{ID: "run", Status: model.RunRunning, DisplayStatus: model.DisplayRunning}
			applyExecutionDisplay(&run, &model.RunScheduling{State: "observed", Execution: execution})
			wantStatus := model.DisplayRunning
			if execution.Phase == "Pending" {
				wantStatus = model.DisplayQueued
			}
			if run.DisplayReason != "waiting for lock ConfigMap/oberth-sync-limits/kvm (0/1 free)" || run.DisplayStatus != wantStatus {
				t.Fatalf("display = %q / %q", run.DisplayStatus, run.DisplayReason)
			}
		})
	}
	run := model.Run{ID: "run", Status: model.RunRunning, DisplayStatus: model.DisplayRunning}
	applyExecutionDisplay(&run, &model.RunScheduling{State: "observed", Execution: &model.ExecutionObservation{State: "observed", Message: "Workflow execution is running."}})
	if run.DisplayReason != "" {
		t.Fatalf("plain running run got a lock reason: %q", run.DisplayReason)
	}
}
