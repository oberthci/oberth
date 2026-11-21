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

// A run stopped or terminated while two of its steps had just taken both units
// of a semaphore ends with no lock status of its own, yet the controller still
// counts one unit for it: the later waiter names it. Lock status never lists
// the ended run as a holder and lists it as stale. A run whose steps retry, or
// fail without a cancel, leaves nothing behind. In the ordinary hand-over the
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
		"default/ConfigMap/oberth-sync-limits/heavy-retry holder waiting-heavy-retry",
		"default/ConfigMap/oberth-sync-limits/heavy-stop holder waiting-heavy-stop",
		"default/ConfigMap/oberth-sync-limits/heavy-stop stale ended-heavy-stop",
		"default/ConfigMap/oberth-sync-limits/heavy-stop waiter waiting-heavy-stop",
		"default/ConfigMap/oberth-sync-limits/heavy-terminate holder waiting-heavy-terminate",
		"default/ConfigMap/oberth-sync-limits/heavy-terminate stale ended-heavy-terminate",
		"default/ConfigMap/oberth-sync-limits/heavy-terminate waiter waiting-heavy-terminate",
		"default/Mutex/release holder mutex-holder",
		"default/Mutex/release waiter mutex-pending-stop",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("lock facts\n got %q\nwant %q", got, want)
	}
}

// A live holder named by a waiter is not stale, and a waiter naming a Workflow
// that is not listed (deleted; the controller drops it itself) adds nothing.
func TestLockFactsStaleOnlyForListedEndedWorkflows(t *testing.T) {
	t.Parallel()
	rows := cancelledLocks(t)
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

// A waiting entry counts only while the Workflow is Pending or a node still
// waits for that lock: once its node holds, the leaked unit's waiter is no
// waiter and its count, however it reads, names nothing stale.
func TestLockFactsWaiterOnlyWhileWaiting(t *testing.T) {
	t.Parallel()
	rows := cancelledLocks(t)
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
	plan := admissioncontract.Plan{Exact: true, Entries: []admissioncontract.PlanEntry{{Path: "e", Name: "e"}, {Path: "f", Name: "f"}}}
	const hint = "Run oberth lock status to see who holds it."
	for id, n := range rows[i].Status.Nodes {
		n.Message = "Waiting for lock. Lock status: 0/2"
		rows[i].Status.Nodes[id] = n
	}
	out := project(t, argojob.Projection{Plan: plan, Workflow: &rows[i]})
	for _, s := range out.Steps {
		waiting := s.DisplayReason == "waiting for a lock"
		if waiting != (s.Path == "f") || strings.Contains(s.Detail, hint) != waiting || waiting && s.Detail != "Waiting for lock. Lock status: 0/2\n"+hint {
			t.Fatalf("step %s: reason %q detail %q", s.Path, s.DisplayReason, s.Detail)
		}
	}
	for _, n := range out.Nodes {
		if strings.Contains(n.Message, hint) {
			t.Fatal("native node message changed", n)
		}
	}
}

// Terminate marks a run that was waiting for a workflow-level lock Succeeded;
// it is reported cancelled, never passed. Stop leaves it queued for the lock.
func TestTerminatedLockWaiterIsCancelledNotPassed(t *testing.T) {
	t.Parallel()
	plan := admissioncontract.Plan{Exact: true, Entries: []admissioncontract.PlanEntry{{Path: "", Name: "main"}}}
	for name, want := range map[string]stepmodel.RunStatus{"mutex-pending-terminate": stepmodel.RunCancelled, "mutex-pending-stop": stepmodel.RunQueued, "mutex-holder": stepmodel.RunRunning} {
		rows := cancelledLocks(t)
		i := slices.IndexFunc(rows, func(w wf.Workflow) bool { return w.Name == name })
		if out := project(t, argojob.Projection{Plan: plan, Workflow: &rows[i]}); out.Status != want {
			t.Fatalf("%s: %s, want %s", name, out.Status, want)
		}
	}
	rows := cancelledLocks(t)
	i := slices.IndexFunc(rows, func(w wf.Workflow) bool { return w.Name == "mutex-pending-terminate" })
	root := rows[i].Status.Nodes[rows[i].Name]
	root.Phase = wf.NodeSucceeded
	rows[i].Status.Nodes[rows[i].Name] = root
	if out := project(t, argojob.Projection{Plan: plan, Workflow: &rows[i]}); out.Status != stepmodel.RunPassed {
		t.Fatal("work that succeeded before the cancel is not passed", out.Status)
	}
}
