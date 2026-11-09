package service

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/api"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/store"
	"github.com/oberthci/oberth/internal/storetest"
)

var agentActor = api.Actor{Identity: "agent@host", Admin: true}

type fakeHoldOperator struct {
	holds []model.AssistHold
}

func (f *fakeHoldOperator) AssistPolicy() model.AssistPolicy {
	return model.AssistPolicy{Enabled: true, HoldTTL: 20 * time.Minute, MaxConcurrentHolds: 2}
}
func (f *fakeHoldOperator) ListAssistHolds(context.Context, string) ([]model.AssistHold, error) {
	return f.holds, nil
}
func (f *fakeHoldOperator) ExecAssistHold(context.Context, model.AssistExecRequest) (model.AssistExecResult, error) {
	return model.AssistExecResult{}, errors.New("unused")
}
func (f *fakeHoldOperator) ReleaseAssistHold(context.Context, string) error { return nil }
func (f *fakeHoldOperator) AbandonAssistHold(context.Context, string) error { return nil }

type fakeSyncObserver struct {
	byRun map[string]*model.SyncObservation
}

func (f *fakeSyncObserver) ObserveScheduling(context.Context, model.Run) (*model.ExecutionObservation, error) {
	return &model.ExecutionObservation{State: "observed"}, nil
}
func (f *fakeSyncObserver) ObserveSynchronization(_ context.Context, run model.Run) (*model.SyncObservation, error) {
	if observation, ok := f.byRun[run.ID]; ok {
		return observation, nil
	}
	return &model.SyncObservation{}, nil
}

type agentFixture struct {
	*controlFixture
	service  *API
	holds    *fakeHoldOperator
	observer *fakeSyncObserver
	recorder *agentEventRecorder
	now      time.Time
}

func newAgentFixture(t *testing.T) *agentFixture {
	t.Helper()
	f := &agentFixture{controlFixture: newControlFixture(t), holds: &fakeHoldOperator{}, observer: &fakeSyncObserver{byRun: map[string]*model.SyncObservation{}},
		now: time.Now().UTC()}
	service, err := NewAPI(APIConfig{
		Runs: f.store, History: f.store, Repositories: f.store, Issues: f.store, Promotions: f.store, PromotionRuns: f.store,
		Enqueues: f.scheduler, Git: f.git, Logs: f.logs, Auditor: f.store, Signals: f.signals,
		AssistHolds: f.holds, RunAssists: f.store, SchedulingObserver: f.observer,
		AgentEvents: f.store, AgentEventPolicy: AgentEventPolicy{LockWait: time.Minute},
		MaximumWait: testTimeout(2 * time.Second),
	})
	if err != nil {
		t.Fatal(err)
	}
	f.service, f.recorder = service, service.agentEvents
	f.recorder.now = func() time.Time { return f.now }
	f.recorder.started = f.now
	return f
}

func (f *agentFixture) enqueue(t *testing.T, ref, seed string, assist bool) model.Run {
	t.Helper()
	enqueued, err := storetest.EnqueueRun(f.store, t.Context(), model.RunSpec{RepoID: f.repo.ID, RefKind: model.RefBranch, Ref: ref,
		SHA: strings.Repeat(seed, 40), Actor: "agent@host", Trigger: "push", Assist: assist})
	if err != nil {
		t.Fatal(err)
	}
	run, err := f.store.Run(t.Context(), enqueued.ID)
	if err != nil {
		t.Fatal(err)
	}
	return run
}

func (f *agentFixture) events(t *testing.T, actor api.Actor, arguments map[string]any) api.EventPage {
	t.Helper()
	raw, _ := json.Marshal(arguments)
	value, err := f.service.CallTool(t.Context(), actor, "events_since", raw)
	if err != nil {
		t.Fatal(err)
	}
	return value.(api.EventPage)
}

// run.finished carries status, failed step, reason and retry hint, and is
// recorded once however often the run is observed.
func TestAgentEventRunFinishedOnceWithReasonAndHint(t *testing.T) {
	f := newAgentFixture(t)
	run := f.enqueue(t, "land/a", "a", false)
	if _, err := f.store.FinishRun(t.Context(), run.ID, model.RunResult{Status: model.RunFailed, Phase: "test", FailedStage: "test", FailedStep: "unit", Error: "exit 1"}); err != nil {
		t.Fatal(err)
	}
	f.recorder.handle(t.Context(), agentNotification{kind: "run", id: run.ID})
	f.recorder.handle(t.Context(), agentNotification{kind: "run", id: run.ID})
	f.recorder.memo = map[string]bool{} // a restarted process sees it again
	f.recorder.sweep(t.Context())
	page := f.events(t, agentActor, map[string]any{})
	if len(page.Events) != 1 || page.Events[0].Kind != model.AgentEventRunFinished {
		t.Fatalf("events = %+v", page.Events)
	}
	event := page.Events[0]
	hint, _ := event.Data["retry_hint"].(map[string]any)
	if event.RunID != run.ID || event.Data["failed_step"] != "unit" || event.Data["status"] != "failed" || hint["class"] != model.RetryCode ||
		!strings.Contains(event.Summary, "land/a") || event.Repo != "codeberg/acme/oberth" {
		t.Fatalf("event = %+v", event)
	}
	if page.NextAfterID != event.ID || page.LatestID != event.ID {
		t.Fatalf("cursor = %+v", page)
	}
}

// run.held: step, pod, held_since, abandon_at and whether the run holds
// several steps, once per parked Pod; a released hold is not reported.
func TestAgentEventRunHeldPayload(t *testing.T) {
	f := newAgentFixture(t)
	run := f.enqueue(t, "land/held", "b", true)
	run.Status, run.WorkflowName = model.RunRunning, "wf-held"
	since := f.now.Add(-time.Minute).Truncate(time.Second)
	f.holds.holds = []model.AssistHold{
		{Pod: "wf-held-test-1", Step: "test", Attempt: 1, HeldSince: since, PatchCapable: true},
		{Pod: "wf-held-lint-1", Step: "lint", Attempt: 1, HeldSince: since},
	}
	f.recorder.observeActive(t.Context(), []model.Run{run})
	f.recorder.observeActive(t.Context(), []model.Run{run})
	page := f.events(t, agentActor, map[string]any{"kinds": []string{model.AgentEventRunHeld}})
	if len(page.Events) != 2 {
		t.Fatalf("held events = %+v", page.Events)
	}
	data := page.Events[0].Data
	if data["step"] != "test" || data["pod"] != "wf-held-test-1" || data["held_since"] != since.Format(time.RFC3339) ||
		data["abandon_at"] != since.Add(20*time.Minute).Format(time.RFC3339) || data["holds_in_run"] != float64(2) || data["multiple_holds"] != true {
		t.Fatalf("held payload = %+v", data)
	}
	if !strings.Contains(page.Events[0].Summary, "2 steps held") {
		t.Fatalf("summary = %q", page.Events[0].Summary)
	}
	// A continued hold is running work, not a hold.
	run.Assist = &model.RunAssistRecord{Events: []model.RunAssistEvent{{Pod: "wf-held-test-1", Step: "test", Kind: model.RunAssistContinue}}}
	f.holds.holds = append(f.holds.holds, model.AssistHold{Pod: "wf-held-test-2", Step: "test", Attempt: 1, HeldSince: since})
	f.holds.holds[0].Pod = "wf-held-test-1"
	f.recorder.observeHolds(t.Context(), model.Run{ID: run.ID, RepoID: run.RepoID, Status: model.RunRunning, WorkflowName: "wf-held", AssistRequested: true,
		Assist: &model.RunAssistRecord{Events: []model.RunAssistEvent{{Pod: "wf-held-test-2", Step: "test", Kind: model.RunAssistContinue}}}})
	if again := f.events(t, agentActor, map[string]any{"kinds": []string{model.AgentEventRunHeld}}); len(again.Events) != 2 {
		t.Fatalf("a released hold was reported: %+v", again.Events)
	}
}

// Lock waits past the threshold become run.lock_wait with the holders; the
// history proves the maximum number of concurrent holders.
func TestAgentEventLockWaitAndSyncStatus(t *testing.T) {
	f := newAgentFixture(t)
	holder := f.enqueue(t, "land/holder", "c", false)
	waiter := f.enqueue(t, "land/waiter", "d", false)
	holder.Status, holder.WorkflowName = model.RunRunning, "wf-holder"
	waiter.Status, waiter.WorkflowName = model.RunRunning, "wf-waiter"
	f.observer.byRun[holder.ID] = &model.SyncObservation{Holding: []string{"oberth-argo/ConfigMap/oberth-sync-limits/kvm"}}
	f.observer.byRun[waiter.ID] = &model.SyncObservation{Waiting: []string{"oberth-argo/ConfigMap/oberth-sync-limits/kvm"}}
	f.recorder.observeActive(t.Context(), []model.Run{holder, waiter})
	if page := f.events(t, agentActor, map[string]any{}); len(page.Events) != 0 {
		t.Fatalf("a short wait was reported: %+v", page.Events)
	}
	f.now = f.now.Add(90 * time.Second)
	f.recorder.observeActive(t.Context(), []model.Run{holder, waiter})
	f.recorder.observeActive(t.Context(), []model.Run{holder, waiter})
	page := f.events(t, agentActor, map[string]any{"kinds": []string{model.AgentEventRunLockWait}})
	if len(page.Events) != 1 {
		t.Fatalf("lock wait events = %+v", page.Events)
	}
	data := page.Events[0].Data
	holders, _ := data["holders"].([]any)
	if data["lock"] != "ConfigMap/oberth-sync-limits/kvm" || len(holders) != 1 || holders[0] != holder.ID || data["waited_s"] != float64(90) {
		t.Fatalf("lock wait = %+v", data)
	}
	// The holder finishes, the waiter takes the lock: never two at once.
	f.now = f.now.Add(10 * time.Second)
	delete(f.observer.byRun, holder.ID)
	f.observer.byRun[waiter.ID] = &model.SyncObservation{Holding: []string{"oberth-argo/ConfigMap/oberth-sync-limits/kvm"}}
	f.recorder.observeActive(t.Context(), []model.Run{waiter})
	raw, _ := json.Marshal(map[string]any{"lock": "ConfigMap/oberth-sync-limits/kvm"})
	value, err := f.service.CallTool(t.Context(), agentActor, "sync_status", raw)
	if err != nil {
		t.Fatal(err)
	}
	status := value.(SyncStatusResponse)
	if !status.Observing || len(status.Locks) != 1 || len(status.Locks[0].Holders) != 1 || status.Locks[0].Holders[0].RunID != waiter.ID || len(status.Locks[0].Waiters) != 0 {
		t.Fatalf("current = %+v", status.Locks)
	}
	if len(status.History) != 1 || status.History[0].MaxConcurrentHolders != 1 || status.History[0].HolderIntervals != 2 ||
		status.History[0].Waits.Count != 1 || status.History[0].Waits.MaxSeconds != 100 {
		t.Fatalf("history = %+v", status.History)
	}
	// Two runs holding at once is measured as such.
	other := f.enqueue(t, "land/other", "e", false)
	other.Status, other.WorkflowName = model.RunRunning, "wf-other"
	f.observer.byRun[other.ID] = &model.SyncObservation{Holding: []string{"oberth-argo/ConfigMap/oberth-sync-limits/kvm"}}
	f.now = f.now.Add(10 * time.Second)
	f.recorder.observeActive(t.Context(), []model.Run{waiter, other})
	value, _ = f.service.CallTool(t.Context(), agentActor, "sync_status", raw)
	if got := value.(SyncStatusResponse).History[0].MaxConcurrentHolders; got != 2 {
		t.Fatalf("max concurrent holders = %d", got)
	}
}

// events_since and sync_status are scoped: an uplink scoped to another
// upstream sees none of these events or lock members.
func TestAgentEventsScopedToUplink(t *testing.T) {
	f := newAgentFixture(t)
	if _, err := storetest.CreateUpstream(f.store, t.Context(), model.UpstreamSpec{Name: "github", Kind: "ssh", BaseURL: "ssh://github.com/acme"}); err != nil {
		t.Fatal(err)
	}
	run := f.enqueue(t, "land/scoped", "f", false)
	if _, err := f.store.FinishRun(t.Context(), run.ID, model.RunResult{Status: model.RunPassed}); err != nil {
		t.Fatal(err)
	}
	f.recorder.handle(t.Context(), agentNotification{kind: "run", id: run.ID})
	running := f.enqueue(t, "land/lock", "9", false)
	running.Status, running.WorkflowName = model.RunRunning, "wf-lock"
	f.observer.byRun[running.ID] = &model.SyncObservation{Holding: []string{"ns/Mutex/oberth-release"}}
	f.recorder.observeActive(t.Context(), []model.Run{running})

	other := api.Actor{Identity: "other@host", Scope: []string{"github"}}
	if page := f.events(t, other, map[string]any{}); len(page.Events) != 0 || page.NextAfterID != page.LatestID {
		t.Fatalf("scoped events = %+v", page)
	}
	own := api.Actor{Identity: "own@host", Scope: []string{"codeberg"}}
	if page := f.events(t, own, map[string]any{}); len(page.Events) != 1 {
		t.Fatalf("own-scope events = %+v", page)
	}
	if page := f.events(t, own, map[string]any{"own": true}); len(page.Events) != 0 {
		t.Fatalf("own:true kept another uplink's run: %+v", page.Events)
	}
	if page := f.events(t, own, map[string]any{"refs_glob": "release/*"}); len(page.Events) != 0 {
		t.Fatalf("refs_glob ignored: %+v", page.Events)
	}
	raw, _ := json.Marshal(map[string]any{"repos": []string{"codeberg/acme/oberth"}})
	if _, err := f.service.CallTool(t.Context(), other, "events_since", raw); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("hidden repository filter = %v", err)
	}
	value, err := f.service.CallTool(t.Context(), other, "sync_status", json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if status := value.(SyncStatusResponse); len(status.Locks) != 0 || len(status.History) != 0 {
		t.Fatalf("scoped sync status leaks: %+v", status)
	}
	value, _ = f.service.CallTool(t.Context(), own, "sync_status", json.RawMessage(`{}`))
	if status := value.(SyncStatusResponse); len(status.Locks) != 1 || status.Locks[0].Lock != "Mutex/oberth-release" {
		t.Fatalf("own sync status = %+v", status)
	}
	if _, err := f.service.CallTool(t.Context(), agentActor, "events_since", json.RawMessage(`{"kinds":["run.bogus"]}`)); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("unknown kind = %v", err)
	}
	if _, err := f.service.CallTool(t.Context(), agentActor, "events_subscribe", json.RawMessage(`{"refs_glob":"["}`)); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("bad glob = %v", err)
	}
	value, err = f.service.CallTool(t.Context(), agentActor, "events_subscribe", json.RawMessage(`{"repos":["oberth"],"kinds":["run.held"]}`))
	if err != nil {
		t.Fatal(err)
	}
	if subscribed := value.(api.EventsSubscribeResponse); len(subscribed.Subscription.Repos) != 1 || subscribed.Subscription.Repos[0] != "codeberg/acme/oberth" || subscribed.Session {
		t.Fatalf("subscription = %+v", subscribed)
	}
}

// Releases and publication failures have their own events; an automatic
// publication retry is reported per attempt with its schedule.
func TestAgentEventReleaseAndPublication(t *testing.T) {
	f := newAgentFixture(t)
	release, err := storetest.EnqueueRun(f.store, t.Context(), model.RunSpec{RepoID: f.repo.ID, RefKind: model.RefTag, Ref: "v1.2.3", SHA: strings.Repeat("7", 40),
		Actor: "agent@host", Trigger: "tag", Release: true})
	if err != nil {
		t.Fatal(err)
	}
	finished, err := f.store.FinishRun(t.Context(), release.ID, model.RunResult{Status: model.RunPassed})
	if err != nil {
		t.Fatal(err)
	}
	f.recorder.observeRun(t.Context(), finished)
	published := f.events(t, agentActor, map[string]any{"kinds": []string{model.AgentEventReleasePublished}})
	if len(published.Events) != 1 || published.Events[0].Data["tag"] != "v1.2.3" {
		t.Fatalf("release events = %+v", published.Events)
	}
	failed := model.Run{ID: release.ID, RepoID: f.repo.ID, Ref: "land/x", SHA: strings.Repeat("7", 40), Status: model.RunFailed, Phase: "publishing",
		Error: "upstream publication infrastructure failure: all 5 attempts exhausted: connection reset", FinishedAt: &f.now}
	f.recorder.memo = map[string]bool{}
	f.recorder.observeRun(t.Context(), failed)
	retry := model.Publication{ID: "pub-1", RepoID: f.repo.ID, RunID: release.ID, Ref: "land/x", ResultSHA: strings.Repeat("7", 40), Attempts: 2,
		RetryAfter: f.now.Add(4 * time.Minute), Error: publicationRetryPrefix + "3/5 queued", Status: model.PublicationPending}
	f.recorder.observePublicationRetry(t.Context(), retry)
	f.recorder.observePublicationRetry(t.Context(), retry)
	page := f.events(t, agentActor, map[string]any{"kinds": []string{model.AgentEventPublicationFailed}})
	if len(page.Events) != 2 || page.Events[0].Data["final"] != true || page.Events[1].Data["final"] != false || page.Events[1].Data["attempt"] != float64(2) {
		t.Fatalf("publication events = %+v", page.Events)
	}
}

// run_wait returns on finished, on held, and at its bound.
func TestRunWaitUntilFinishedHeldAndTimeout(t *testing.T) {
	f := newAgentFixture(t)
	run := f.enqueue(t, "land/wait", "8", true)
	call := func(arguments map[string]any) (RunWaitResponse, error) {
		raw, _ := json.Marshal(arguments)
		value, err := f.service.CallTool(t.Context(), agentActor, "run_wait", raw)
		response, _ := value.(RunWaitResponse)
		return response, err
	}
	if response, err := call(map[string]any{"id": run.ID, "timeout_s": 1}); err != nil || response.State != runWaitTimeout {
		t.Fatalf("timeout = %+v, %v", response, err)
	}
	if _, err := call(map[string]any{"id": run.ID, "until": "sometime"}); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("bad until = %v", err)
	}
	done := make(chan RunWaitResponse, 1)
	go func() {
		response, _ := call(map[string]any{"id": run.ID, "until": "finished", "timeout_s": 2})
		done <- response
	}()
	time.Sleep(50 * time.Millisecond)
	if _, err := f.store.FinishRun(t.Context(), run.ID, model.RunResult{Status: model.RunFailed, Phase: "infrastructure", Error: "deliver executor identity failed"}); err != nil {
		t.Fatal(err)
	}
	f.signals.NotifyRun(run.ID)
	response := <-done
	if response.State != runWaitFinished || response.Run.Status != model.RunFailed || response.RetryHint == nil || response.RetryHint.Class != model.RetryInfrastructure {
		t.Fatalf("finished = %+v", response)
	}
	if _, err := call(map[string]any{"id": strings.Repeat("e", 32)}); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("missing run = %v", err)
	}
}

func TestRunWaitReturnsOnHold(t *testing.T) {
	f := newAgentFixture(t)
	run := f.enqueue(t, "land/hold", "6", true)
	claimed, err := f.store.ClaimNextRun(t.Context())
	if err != nil || claimed.ID != run.ID {
		t.Fatalf("claim = %+v, %v", claimed, err)
	}
	if _, err := f.store.SetRunWorkflowName(t.Context(), run.ID, "wf-hold"); err != nil {
		t.Fatal(err)
	}
	current, _ := f.store.Run(t.Context(), run.ID)
	if current.Status != model.RunRunning {
		t.Skipf("fixture run is %s, not running", current.Status)
	}
	f.holds.holds = []model.AssistHold{{Pod: "wf-hold-test-1", Step: "test", Attempt: 1, HeldSince: time.Now()}}
	value, err := f.service.CallTool(t.Context(), agentActor, "run_wait", json.RawMessage(`{"id":"`+run.ID+`","until":"held","timeout_s":2}`))
	if err != nil {
		t.Fatal(err)
	}
	if response := value.(RunWaitResponse); response.State != runWaitHeld || len(response.Holds) != 1 || response.Holds[0].Step != "test" {
		t.Fatalf("held = %+v", response)
	}
}

func TestRetryHintClassification(t *testing.T) {
	failed := func(phase, step, message string) model.Run {
		return model.Run{Status: model.RunFailed, Phase: phase, FailedStep: step, Error: message}
	}
	for _, test := range []struct {
		name         string
		run          model.Run
		class, cause string
	}{
		{"publication transport", failed("publishing", "", "upstream publication infrastructure failure: all 5 attempts exhausted: connection reset by peer"), model.RetryInfrastructure, "git_transport"},
		{"publication rejected", failed("publishing", "", "push: non-fast-forward"), model.RetryCode, "upstream_rejected"},
		{"eviction", failed("infrastructure", "", "Evicted: The node was low on resource: memory."), model.RetryInfrastructure, "eviction"},
		{"storage eviction", failed("infrastructure", "", "Evicted: Usage of EmptyDir volume \"x\" exceeds the limit"), model.RetryCode, "resource_limit"},
		{"executor identity", failed("infrastructure", "", "deliver executor identity failed (stage=x reason=y attempts=3)"), model.RetryInfrastructure, "executor_identity"},
		{"image pull", failed("test", "unit", "ErrImagePull: pull access denied"), model.RetryInfrastructure, "image_pull"},
		{"oom", failed("test", "unit", "OOM kill at test/unit: memory limit 1Gi; sampled memory peak 1Gi (lower bound). Inspect oberth run resources --run r --include-failed; change resources in git."), model.RetryCode, "oom"},
		{"deadline artifact", failed("test", "unit", model.ExecutionDeadlineMessage+" Step test/unit: deadline limit 1650s; observed runtime 3m2s; sampled memory peak unknown (lower bound). Inspect oberth run resources --run r --include-failed; change deadlines in git."), model.RetryInfrastructure, "deadline_artifact"},
		{"deadline", failed("test", "unit", model.ExecutionDeadlineMessage+" Step test/unit: deadline limit 600s; observed runtime 10m0s; sampled memory peak unknown (lower bound). Inspect oberth run resources --run r --include-failed; change deadlines in git."), model.RetryCode, "deadline"},
		{"infrastructure", failed("infrastructure", "", "workflow error"), model.RetryInfrastructure, "infrastructure"},
		{"step", failed("test", "unit", "exit 1"), model.RetryCode, "step_failed"},
		{"interrupted", model.Run{Status: model.RunInterrupted, Phase: "restart"}, model.RetryInfrastructure, "interrupted"},
		{"auto retried", model.Run{Status: model.RunFailed, Phase: "infrastructure", AutoRetriedBy: "next"}, model.RetryInfrastructure, "auto_retried"},
	} {
		hint := runRetryHint(test.run, nil)
		if hint == nil || hint.Class != test.class || hint.Cause != test.cause {
			t.Errorf("%s: hint = %+v, want %s/%s", test.name, hint, test.class, test.cause)
		}
	}
	for _, run := range []model.Run{{Status: model.RunPassed}, {Status: model.RunRunning}, {Status: model.RunInterrupted, Phase: "cancelled"}} {
		if hint := runRetryHint(run, nil); hint != nil {
			t.Errorf("%+v: hint = %+v, want none", run, hint)
		}
	}
	retrying := &model.Publication{Status: model.PublicationPending, Attempts: 2, RetryAfter: time.Now().Add(time.Minute)}
	if hint := runRetryHint(model.Run{Status: model.RunRunning}, retrying); hint == nil || hint.Publication == nil || hint.Publication.Attempt != 2 || hint.Publication.MaxAttempts != 5 || !hint.Publication.Automatic {
		t.Errorf("publication retry hint = %+v", hint)
	}
}

func TestAgentEventPolicyBounds(t *testing.T) {
	policy := AgentEventPolicy{Interval: time.Millisecond, Retention: -1}.Effective()
	if policy.Interval != MinAgentEventInterval || policy.Retention != DefaultAgentEventRetention || policy.SyncRetention != DefaultAgentSyncRetention || policy.LockWait != DefaultAgentEventLockWait {
		t.Fatalf("effective = %+v", policy)
	}
	if got := (AgentEventPolicy{Interval: time.Hour, Retention: 1 << 30}).Effective(); got.Interval != MaxAgentEventInterval || got.Retention != MaxAgentEventRetention {
		t.Fatalf("clamped = %+v", got)
	}
	// Without a store the tools refuse instead of pretending to be empty.
	plain, err := NewAPI(APIConfig{Runs: newControlFixture(t).store})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := plain.CallTool(t.Context(), agentActor, "events_since", json.RawMessage(`{}`)); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("no store = %v", err)
	}
	if plain.AgentEventWake() != nil {
		t.Fatal("wake without a recorder")
	}
}

// The sweep's abandon record (v0.18.2) becomes one run.hold_abandoned with
// the reason, held_since and the failed attempt's exit code.
func TestAgentEventRunHoldAbandoned(t *testing.T) {
	f := newAgentFixture(t)
	run := f.enqueue(t, "land/abandoned", "5", true)
	held := f.now.Add(-21 * time.Minute)
	exit := 1
	abandon := model.AssistAbandon{RunID: run.ID, Pod: "wf-test-1", Step: "test", Reason: model.AssistAbandonTTL, HeldSince: held, AbandonedAt: f.now, FailedExitCode: &exit}
	if _, err := f.store.AppendRunAssist(t.Context(), model.RunAssistRequest{RunID: run.ID, Event: abandon.Event()}); err != nil {
		t.Fatal(err)
	}
	f.recorder.handle(t.Context(), agentNotification{kind: "run", id: run.ID})
	f.recorder.handle(t.Context(), agentNotification{kind: "run", id: run.ID})
	page := f.events(t, agentActor, map[string]any{"kinds": []string{model.AgentEventRunHoldAbandoned}})
	if len(page.Events) != 1 {
		t.Fatalf("abandon events = %+v", page.Events)
	}
	data := page.Events[0].Data
	if data["step"] != "test" || data["pod"] != "wf-test-1" || data["abandon_reason"] != "ttl" || data["abandoned_by"] != "oberth" ||
		data["held_since"] != held.UTC().Format(time.RFC3339) || data["failed_exit_code"] != float64(1) || !strings.Contains(page.Events[0].Summary, "exited 1") {
		t.Fatalf("abandon event = %+v", page.Events[0])
	}
}
