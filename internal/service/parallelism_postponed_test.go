package service

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"
)

// A Workflow the Argo controller postpones for its parallelism is reported
// as a waiter on the parallelism key: sync_status shows it with the admitted
// Oberth Workflows as holders, and a wait past the threshold is one
// run.lock_wait event on that key, marked postponed (#1517).
func TestAgentEventRunPostponedAndSyncStatus(t *testing.T) {
	f := newAgentFixture(t)
	running := f.enqueue(t, "land/running", "c", false)
	postponed := f.enqueue(t, "land/postponed", "d", false)
	running.Status, running.WorkflowName = model.RunRunning, "wf-running"
	postponed.Status, postponed.WorkflowName = model.RunRunning, "wf-postponed"
	f.observer.byRun[running.ID] = &model.SyncObservation{Holding: []string{model.SyncParallelismLock}}
	f.observer.byRun[postponed.ID] = &model.SyncObservation{Waiting: []string{model.SyncParallelismLock}}
	f.recorder.observeActive(t.Context(), []model.Run{running, postponed})
	f.now = f.now.Add(90 * time.Second)
	f.recorder.observeActive(t.Context(), []model.Run{running, postponed})
	f.recorder.observeActive(t.Context(), []model.Run{running, postponed})
	page := f.events(t, agentActor, map[string]any{"kinds": []string{model.AgentEventRunLockWait}})
	if len(page.Events) != 1 || page.Events[0].RunID != postponed.ID {
		t.Fatalf("postponed events = %+v", page.Events)
	}
	data := page.Events[0].Data
	if data["lock"] != model.SyncParallelismLock || data["postponed"] != true || data["waited_s"] != float64(90) || data["running"] != float64(1) {
		t.Fatalf("postponed event = %+v", data)
	}
	raw, _ := json.Marshal(map[string]any{"lock": model.SyncParallelismLock})
	value, err := f.service.CallTool(t.Context(), agentActor, "sync_status", raw)
	if err != nil {
		t.Fatal(err)
	}
	status := value.(SyncStatusResponse)
	if len(status.Locks) != 1 || len(status.Locks[0].Holders) != 1 || status.Locks[0].Holders[0].RunID != running.ID ||
		len(status.Locks[0].Waiters) != 1 || status.Locks[0].Waiters[0].RunID != postponed.ID || status.Locks[0].Waiters[0].Seconds != 90 {
		t.Fatalf("parallelism view = %+v", status.Locks)
	}
}

// The reason line names the Argo parallelism wait in fixed words.
func TestLockWaitReasonNamesParallelismPostponement(t *testing.T) {
	got := lockWaitReason(&model.ExecutionObservation{Message: model.ParallelismWaitMessage})
	if got != "waiting for an Argo workflow slot (controller parallelism)" {
		t.Fatalf("reason = %q", got)
	}
	run := model.Run{Status: model.RunRunning}
	applyExecutionDisplay(&run, &model.RunScheduling{Execution: &model.ExecutionObservation{State: "observed", Phase: "Pending", Message: model.ParallelismWaitMessage}})
	if run.DisplayReason == "" || run.Status != model.RunRunning {
		t.Fatalf("display = %q, status %s", run.DisplayReason, run.Status)
	}
}
