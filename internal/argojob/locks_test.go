package argojob_test

import (
	"encoding/json"
	"os"
	"slices"
	"strings"
	"testing"

	wf "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"

	"github.com/oberthci/oberth/internal/admissioncontract"
	"github.com/oberthci/oberth/internal/argojob"
	"github.com/oberthci/oberth/internal/stepmodel"
)

// cancelledLocks reads what the pinned controller left after the runs of
// pkg/argoworkflow/testdata/cancelled_locks_test.go ended.
func cancelledLocks(t *testing.T) []wf.Workflow {
	t.Helper()
	raw, err := os.ReadFile("testdata/cancelled-locks.json")
	if err != nil {
		t.Fatal(err)
	}
	var rows []wf.Workflow
	if err = json.Unmarshal(raw, &rows); err != nil {
		t.Fatal(err)
	}
	return rows
}

// facts renders LockFacts as "lock role workflow" lines for comparison.
func facts(t *testing.T, rows []wf.Workflow) []string {
	t.Helper()
	read, err := argojob.LockFacts(t.Context(), rows)
	if err != nil {
		t.Fatal(err)
	}
	out := []string{}
	for _, f := range read {
		out = append(out, f.Lock+" "+string(f.Role)+" "+f.Workflow.Name)
	}
	slices.Sort(out)
	return out
}

// Normal Stop/Terminate and failure release both units in the pinned controller.
// A later run takes both units while its third step waits. No ended run is
// listed as a holder or stale. In the ordinary hand-over the
// run that took the unit keeps its waiting entry and is no waiter, and the
// waiter not yet woken still names the ended holder, which is not stale because
// that count does not name the live holder.
func TestLockFactsOfTheControllerWriteForCancelledRuns(t *testing.T) {
	t.Parallel()
	got := facts(t, cancelledLocks(t))
	want := []string{
		"default/ConfigMap/oberth-sync-limits/handover holder handover-b",
		"default/ConfigMap/oberth-sync-limits/handover waiter handover-c",
		"default/ConfigMap/oberth-sync-limits/heavy-failed holder waiting-heavy-failed",
		"default/ConfigMap/oberth-sync-limits/heavy-failed waiter waiting-heavy-failed",
		"default/ConfigMap/oberth-sync-limits/heavy-retry holder waiting-heavy-retry",
		"default/ConfigMap/oberth-sync-limits/heavy-retry waiter waiting-heavy-retry",
		"default/ConfigMap/oberth-sync-limits/heavy-stop holder waiting-heavy-stop",
		"default/ConfigMap/oberth-sync-limits/heavy-stop waiter waiting-heavy-stop",
		"default/ConfigMap/oberth-sync-limits/heavy-terminate holder waiting-heavy-terminate",
		"default/ConfigMap/oberth-sync-limits/heavy-terminate waiter waiting-heavy-terminate",
		"default/Mutex/release holder mutex-holder",
		"default/Mutex/release waiter mutex-pending-stop",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("lock facts\n got %q\nwant %q", got, want)
	}
}

// A stale count can still outlive a cancelled run. Inject that observation
// explicitly; normal cancellation no longer produces it in this controller.
func leakedLocks(t *testing.T) []wf.Workflow {
	t.Helper()
	rows := cancelledLocks(t)
	i := slices.IndexFunc(rows, func(w wf.Workflow) bool { return w.Name == "waiting-heavy-stop" })
	waiting := &rows[i].Status.Synchronization.Semaphore.Waiting[0]
	waiting.Holders = append(waiting.Holders, "default/ended-heavy-stop/stale")
	if got := facts(t, rows); !slices.Contains(got, "default/ConfigMap/oberth-sync-limits/heavy-stop stale ended-heavy-stop") {
		t.Fatal("cancelled Workflow named by a current count not listed stale", got)
	}
	return rows
}

// A live holder named by a waiter is not stale, and a waiter naming a Workflow
// that is not listed (deleted; the controller drops it itself) adds nothing.
func TestLockFactsStaleOnlyForListedEndedWorkflows(t *testing.T) {
	t.Parallel()
	rows := leakedLocks(t)
	live := slices.IndexFunc(rows, func(w wf.Workflow) bool { return w.Name == "ended-heavy-stop" })
	rows[live].Status.Phase = wf.WorkflowRunning
	if got := facts(t, rows); slices.Contains(got, "default/ConfigMap/oberth-sync-limits/heavy-stop stale ended-heavy-stop") {
		t.Fatal("running Workflow reported stale", got)
	}
	rows = slices.Delete(rows, live, live+1)
	for _, f := range facts(t, rows) {
		if strings.HasSuffix(f, " ended-heavy-stop") {
			t.Fatal("deleted Workflow reported", f)
		}
	}
}

// Before the head waiter of an ordinary hand-over takes the unit, no live
// holder exists and the other waiter's count still names the run that ended by
// itself; that run released its unit, so nothing is stale. The same run, had it
// been stopped, would be.
func TestLockFactsRunEndedByItselfIsNeverStale(t *testing.T) {
	t.Parallel()
	rows := cancelledLocks(t)
	head := slices.IndexFunc(rows, func(w wf.Workflow) bool { return w.Name == "handover-b" })
	rows = slices.Delete(rows, head, head+1)
	const stale = "default/ConfigMap/oberth-sync-limits/handover stale handover-a"
	if got := facts(t, rows); slices.Contains(got, stale) {
		t.Fatal("run that ended by itself listed stale", got)
	}
	ended := slices.IndexFunc(rows, func(w wf.Workflow) bool { return w.Name == "handover-a" })
	rows[ended].Spec.Shutdown = wf.ShutdownStrategyStop
	if got := facts(t, rows); !slices.Contains(got, stale) {
		t.Fatal("stopped run named by a current count not listed stale", got)
	}
}

// A later run that holds the lock after the waiter's last write hides a real
// leak: the count does not name it, so nothing is listed. Silence, not a
// false remedy, is the error this reading allows.
func TestLockFactsLeakUnlistedBehindALaterHolder(t *testing.T) {
	t.Parallel()
	rows := leakedLocks(t)
	i := slices.IndexFunc(rows, func(w wf.Workflow) bool { return w.Name == "waiting-heavy-stop" })
	late := *rows[i].DeepCopy()
	late.Name, late.Status.Nodes = "late", nil
	late.Status.Synchronization = &wf.SynchronizationStatus{Semaphore: &wf.SemaphoreStatus{Holding: []wf.SemaphoreHolding{{Semaphore: "default/ConfigMap/oberth-sync-limits/heavy-stop", Holders: []string{"default/late/late-1"}}}}}
	got := facts(t, append(rows, late))
	if slices.Contains(got, "default/ConfigMap/oberth-sync-limits/heavy-stop stale ended-heavy-stop") || !slices.Contains(got, "default/ConfigMap/oberth-sync-limits/heavy-stop holder late") {
		t.Fatal("a count older than a live holder was trusted", got)
	}
}

// A waiting entry counts only while the Workflow is Pending or a node still
// waits for that lock: once its node holds, the leaked unit's waiter is no
// waiter and its count, however it reads, names nothing stale.
func TestLockFactsWaiterOnlyWhileWaiting(t *testing.T) {
	t.Parallel()
	rows := leakedLocks(t)
	i := slices.IndexFunc(rows, func(w wf.Workflow) bool { return w.Name == "waiting-heavy-stop" })
	for id, n := range rows[i].Status.Nodes {
		n.SynchronizationStatus = nil
		rows[i].Status.Nodes[id] = n
	}
	for _, f := range facts(t, rows) {
		if strings.HasPrefix(f, "default/ConfigMap/oberth-sync-limits/heavy-stop ") && !strings.HasSuffix(f, " holder waiting-heavy-stop") {
			t.Fatal("a Workflow that no longer waits produced", f)
		}
	}
}

// A step waiting for a lock points to lock status once, after the native
// detail; a step of the same run that holds the lock does not.
func TestLockWaitingStepPointsToLockStatus(t *testing.T) {
	t.Parallel()
	rows := cancelledLocks(t)
	i := slices.IndexFunc(rows, func(w wf.Workflow) bool { return w.Name == "waiting-heavy-stop" })
	plan := admissioncontract.Plan{Exact: true, Entries: []admissioncontract.PlanEntry{{Path: "e", Name: "e"}, {Path: "f", Name: "f"}, {Path: "g", Name: "g"}}}
	const hint = "Run oberth lock status to see who holds it."
	for id, n := range rows[i].Status.Nodes {
		n.Message = "Waiting for lock. Lock status: 0/2"
		rows[i].Status.Nodes[id] = n
	}
	out := project(t, argojob.Projection{Plan: plan, Workflow: &rows[i]})
	for _, s := range out.Steps {
		waiting := s.DisplayReason == "waiting for sync limit heavy-stop"
		if waiting != (s.Path == "g") || strings.Contains(s.Detail, hint) != waiting || waiting && s.Detail != "Waiting for lock. Lock status: 0/2\n"+hint {
			t.Fatalf("step %s: reason %q detail %q", s.Path, s.DisplayReason, s.Detail)
		}
	}
	for _, n := range out.Nodes {
		if strings.Contains(n.Message, hint) {
			t.Fatal("native node message changed", n)
		}
	}
}

// A run pending on a workflow-level lock points to lock status after its
// native message; a running holder of that lock does not.
func TestLockWaitingRunPointsToLockStatus(t *testing.T) {
	t.Parallel()
	plan := admissioncontract.Plan{Exact: true, Entries: []admissioncontract.PlanEntry{{Path: "", Name: "main"}}}
	const hint = "Run oberth lock status to see who holds it."
	for name, want := range map[string]string{"mutex-pending-stop": "Waiting for release lock\n" + hint, "mutex-holder": "Waiting for release lock"} {
		rows := cancelledLocks(t)
		i := slices.IndexFunc(rows, func(w wf.Workflow) bool { return w.Name == name })
		rows[i].Status.Message = "Waiting for release lock"
		if out := project(t, argojob.Projection{Plan: plan, Workflow: &rows[i]}); out.Detail != want {
			t.Fatalf("%s: detail %q, want %q", name, out.Detail, want)
		}
	}
}

// Terminate fails a run that was waiting for a workflow-level lock;
// it is reported cancelled. Stop leaves it queued for the lock.
// The holder is queued too while its only Pod has not started.
func TestTerminatedLockWaiterIsCancelledNotPassed(t *testing.T) {
	t.Parallel()
	plan := admissioncontract.Plan{Exact: true, Entries: []admissioncontract.PlanEntry{{Path: "", Name: "main"}}}
	for name, want := range map[string]stepmodel.RunStatus{"mutex-pending-terminate": stepmodel.RunCancelled, "mutex-pending-stop": stepmodel.RunQueued, "mutex-holder": stepmodel.RunQueued} {
		rows := cancelledLocks(t)
		i := slices.IndexFunc(rows, func(w wf.Workflow) bool { return w.Name == name })
		if out := project(t, argojob.Projection{Plan: plan, Workflow: &rows[i]}); out.Status != want {
			t.Fatalf("%s: %s, want %s", name, out.Status, want)
		}
	}
	rows := cancelledLocks(t)
	i := slices.IndexFunc(rows, func(w wf.Workflow) bool { return w.Name == "mutex-pending-terminate" })
	rows[i].Status.Nodes = wf.Nodes{rows[i].Name: {ID: rows[i].Name, Name: rows[i].Name, Type: wf.NodeTypePod, Phase: wf.NodeSucceeded}}
	rows[i].Status.Phase = wf.WorkflowSucceeded
	if out := project(t, argojob.Projection{Plan: plan, Workflow: &rows[i]}); out.Status != stepmodel.RunCancelled || out.Steps[0].Status != stepmodel.StepPassed {
		t.Fatal("cancel lost the run verdict or successful step", out)
	}
}
