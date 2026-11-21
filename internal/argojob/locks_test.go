package argojob_test

import (
	"encoding/json"
	"os"
	"slices"
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
func facts(rows []wf.Workflow) []string {
	out := []string{}
	for _, f := range argojob.LockFacts(rows) {
		out = append(out, f.Lock+" "+string(f.Role)+" "+f.Workflow.Name)
	}
	slices.Sort(out)
	return out
}

// A run stopped or terminated while two of its steps had just taken both units
// of a semaphore ends with no lock status of its own, yet the controller still
// counts one unit for it: the later waiter names it. Lock status never lists
// the ended run as a holder and lists it as stale. A run whose steps retry, or
// fail without a cancel, leaves nothing behind.
func TestLockFactsOfTheControllerWriteForCancelledRuns(t *testing.T) {
	t.Parallel()
	got := facts(cancelledLocks(t))
	want := []string{
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
	if got := facts(rows); slices.Contains(got, "default/ConfigMap/oberth-sync-limits/heavy-stop stale ended-heavy-stop") {
		t.Fatal("running Workflow reported stale", got)
	}
	rows = slices.Delete(rows, live, live+1)
	for _, f := range argojob.LockFacts(rows) {
		if f.Workflow.Name == "ended-heavy-stop" {
			t.Fatal("deleted Workflow reported", f)
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
