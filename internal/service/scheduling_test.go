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
	"github.com/oberthci/oberth/pkg/periapsis"
)

type schedulingObserverFunc func(context.Context, model.Run) (*model.ExecutionObservation, error)

type admittedObservation struct{}

func (admittedObservation) AdmissionSnapshot(int64) *model.AdmissionObservation {
	return &model.AdmissionObservation{State: "admitted", Weight: 1, Capacity: 1, Used: 1}
}

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

func TestClaimedRunShowsCapacityWaitUntilSubmission(t *testing.T) {
	gate := newWeightedAdmission(3)
	scheduler := &Scheduler{admission: gate}
	first := gate.reserve(1)
	releaseFirst, err := first.acquire(t.Context(), periapsis.L)
	if err != nil {
		t.Fatal(err)
	}
	defer releaseFirst()
	second := gate.reserve(2)
	acquired := make(chan func(), 1)
	go acquireForTest(second, periapsis.M, acquired)
	assertNotAcquired(t, acquired)
	service := &API{admission: scheduler, schedulingObserver: schedulingObserverFunc(func(context.Context, model.Run) (*model.ExecutionObservation, error) {
		t.Fatal("unsubmitted run reached workflow observation")
		return nil, nil
	})}
	run := model.Run{Status: model.RunRunning, QueueSequence: 2, QueuePosition: 1, JobName: "reserved-workflow"}
	observation := service.scheduling(t.Context(), run, nil)
	if observation.State != "waiting" || observation.Admission == nil || observation.Admission.Weight != 2 ||
		observation.Admission.Used != 3 || observation.Admission.Capacity != 3 || observation.Admission.Position != 1 ||
		!strings.Contains(observation.Message, "Need 2 weight; 3/3 used; queue position 1") {
		t.Fatalf("capacity wait was not visible: %#v", observation)
	}
	releaseFirst()
	releaseSecond := awaitAcquired(t, acquired)
	defer releaseSecond()
	observation = service.scheduling(t.Context(), run, nil)
	if observation.State != "waiting" || !strings.Contains(observation.Message, "waiting for workflow submission") {
		t.Fatalf("admission alone started run: %#v", observation)
	}
}

func TestSubmittedKVMPreflightHasDistinctSchedulingState(t *testing.T) {
	f, service, intent := kvmReadFixture(t)
	service.admission = admittedObservation{}
	service.logs = f.logs
	ctx := t.Context()
	run, err := f.store.Run(ctx, intent.RunID)
	if err != nil {
		t.Fatal(err)
	}
	check := func(want string) {
		t.Helper()
		observation := service.scheduling(ctx, run, nil)
		if observation == nil || observation.State != want || observation.Admission == nil || run.StartedAt != nil {
			t.Fatalf("preflight state = %#v, run = %#v; want %s", observation, run, want)
		}
	}
	check("waiting")
	if _, err := f.store.ReserveKVMPreflight(ctx, intent); err != nil {
		t.Fatal(err)
	}
	check("waiting")
	if err := f.store.SubmitKVMPreflight(ctx, intent.RunID); err != nil {
		t.Fatal(err)
	}
	check("preflight")
	detail, err := service.Run(ctx, api.Actor{Identity: "agent@host"}, run.ID)
	if err != nil || detail.(RunDetailResponse).Scheduling.State != "preflight" {
		t.Fatalf("run detail hid preflight: %#v, %v", detail, err)
	}
	log, err := service.RunLogLive(ctx, api.Actor{Identity: "agent@host"}, run.ID, 0)
	if err != nil || log.(LiveLogResponse).Status != "waiting" {
		t.Fatalf("live log status diverged from visible run: %#v, %v", log, err)
	}
	if err := f.store.BindKVMPreflight(ctx, intent.RunID, "workflow-uid"); err != nil {
		t.Fatal(err)
	}
	check("preflight")
	if err := f.store.CompleteKVMPreflightCleanup(ctx, intent.RunID, "workflow-uid"); err != nil {
		t.Fatal(err)
	}
	check("waiting")
}

func TestSchedulingProjectionSkipsTerminalAndProgressedListRows(t *testing.T) {
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
		if result := service.observeScheduling(t.Context(), model.Run{Status: model.RunRunning}, []model.StepResult{{Status: state}}, false); result != nil {
			t.Fatalf("progressed=%#v", result)
		}
	}
}

func TestSchedulingProjectionStartupUnavailableAndContext(t *testing.T) {
	service := &API{}
	started := time.Now().UTC()
	run := model.Run{Status: model.RunRunning, StartedAt: &started}
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
	if _, err = fixture.store.SetRunJobName(ctx, enqueued.ID, "wf"); err != nil {
		t.Fatal(err)
	}
	run, err := fixture.store.MarkRunSubmitted(ctx, enqueued.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = fixture.store.PutStepResult(ctx, model.StepResult{RunID: run.ID, Burn: "test", Step: "unit", Status: model.StepPassed, DeclaredSize: periapsis.Size("S")}); err != nil {
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
	if status.Status != "running" || status.DisplayStatus != "deadline-exceeded" || status.Scheduling.Execution.Reason != "DeadlineExceeded" {
		t.Fatalf("status = %+v", status)
	}
	value, err := service.CallTool(ctx, api.Actor{Identity: "agent@host"}, "run_get", json.RawMessage(`{"id":"`+run.ID+`"}`))
	if err != nil {
		t.Fatal(err)
	}
	detail := value.(RunDetailResponse)
	if detail.Run.DisplayStatus != "deadline-exceeded" || *detail.Scheduling.Execution.ReconciliationLagSeconds != 3080 {
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
	if got := presentationRun(terminal); got.DisplayStatus != "deadline-exceeded" {
		t.Fatal("retained terminal deadline reason hidden")
	}
}
