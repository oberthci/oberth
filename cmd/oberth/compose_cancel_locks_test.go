//go:build linux

// These tests exercise the Linux server and Git receive path.

package main

import (
	"encoding/json"
	core "k8s.io/api/core/v1"
	"os"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"

	wf "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/nativebinding"
	"github.com/oberthci/oberth/internal/stateport"
	"github.com/oberthci/oberth/internal/stepmodel"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// cancelledLocks is the pinned controller's write for a run cancelled while
// its steps held both units of a semaphore (pkg/argoworkflow checks it).
const cancelledLocks = "../../internal/argojob/testdata/cancelled-locks.json"

// A run cancelled while holding a semaphore is reported cancelled only once its
// Workflow has ended; lock status then never lists it as a holder. An actual
// stale count, injected separately, names the command that clears it.
func TestComposedCancelledRunLeavesNoLockHolder(t *testing.T) {
	t.Parallel()
	f := composeFixture(t, composeBinary(t))
	f.prepareCommit(t)
	f.prepareAdministrator(t)
	cancel, done := f.run(t)
	defer cancel()
	f.checkStarting(t, done)
	f.waitStarted(t, done)
	f.bindClient(t)
	if pushed, err := f.pushResult(t, "refs/heads/cancel-holding", "-o", "oberth.run=1"); err != nil {
		t.Fatal("push refused", pushed, err)
	}
	run := f.waitRun(t, func(r stateport.Run) bool { return r.Origin.WorkflowUID != "" })
	ended, waiting := f.controllerWrites(t, run)
	lock := waiting.Status.Synchronization.Semaphore.Waiting[0].Semaphore
	held := f.nativeWorkflow(t, run)
	key := held.Namespace + "/" + held.Name + "/" + held.Name
	held.Status = wf.WorkflowStatus{Phase: wf.WorkflowRunning, Synchronization: &wf.SynchronizationStatus{Semaphore: &wf.SemaphoreStatus{Holding: []wf.SemaphoreHolding{{Semaphore: lock, Holders: []string{key + "-a", key + "-b"}}}}}}
	held.Status.Nodes = wf.Nodes{held.Name + "-a": {DisplayName: "compile"}, held.Name + "-b": {DisplayName: "test"}}
	f.updateNative(t, held)
	if _, err := f.kube.CoreV1().ConfigMaps(held.Namespace).Create(t.Context(), &core.ConfigMap{ObjectMeta: meta.ObjectMeta{Name: "oberth-sync-limits"}, Data: map[string]string{"heavy-stop": "2"}}, meta.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	want := apicontract.LockState{Lock: lock, Held: 2, Limit: 2, Usage: "2/2", Holders: []apicontract.LockHolder{{RunID: run.ID, Permits: 2, Steps: "compile, test"}}, Waiters: []string{}}
	if got := f.lockStatus(t, lock); !reflect.DeepEqual(got, want) {
		t.Fatal("holding run is not the holder", got)
	}
	f.cancelStillRunning(t, run)
	stopped := f.nativeWorkflow(t, run)
	stopped.Status = ended.Status
	f.updateNative(t, stopped)
	if _, err := f.argo.ArgoprojV1alpha1().Workflows(f.config.PipelineNamespace).Create(t.Context(), waiting, meta.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	f.waitRun(t, func(r stateport.Run) bool { return r.Status == stepmodel.RunCancelled })
	f.checkCancelledLockStatus(t, run, waiting, key)
}

func (f *composedFixture) checkCancelledLockStatus(t *testing.T, run stateport.Run, waiting *wf.Workflow, key string) {
	t.Helper()
	lock := waiting.Status.Synchronization.Semaphore.Waiting[0].Semaphore
	// The later run holds both released units with e and f while g waits.
	got := f.lockStatus(t, lock)
	if !slices.Equal(lockHolderRuns(got.Holders), []string{"waiting-heavy-stop"}) || !slices.Equal(got.Waiters, []string{"waiting-heavy-stop"}) || len(got.Stale) != 0 || got.Usage != "2/2" {
		t.Fatal("cancelled run is still listed as holding a unit", got)
	}
	if got.Remedy != "" || got.Command != "" {
		t.Fatal("released units produced a stale-lock remedy", got)
	}
	// Preserve the diagnostic path for a cancelled holder named by a current
	// count, without claiming that this normal shutdown still leaks a unit.
	waiting.Status.Synchronization.Semaphore.Waiting[0].Holders = append(waiting.Status.Synchronization.Semaphore.Waiting[0].Holders, key+"-stale")
	f.updateNative(t, waiting)
	got = f.lockStatus(t, lock)
	if !slices.Equal(got.Stale, []string{run.ID}) {
		t.Fatal("cancelled holder in a current count is hidden", got)
	}
	if got.Remedy == "" || got.Command != "kubectl -n "+f.config.ArgoNamespace+" rollout restart deployment/oberth-controller-workflow-controller" {
		t.Fatal("stale unit does not name the command that clears it", got)
	}
}

// cancelStillRunning cancels through the command line; the run stays queued
// until its Workflow ends because no step has been observed running.
func (f *composedFixture) cancelStillRunning(t *testing.T, run stateport.Run) {
	t.Helper()
	app, out, diagnostic := f.revokeApplication(t)
	if code := app.run(t.Context(), []string{"run", "cancel", run.ID, "--reason", "free the lock"}); code != 0 {
		t.Fatal("cancel failed", code, out.String(), diagnostic.String())
	}
	if f.nativeWorkflow(t, run).Spec.Shutdown != wf.ShutdownStrategyTerminate {
		t.Fatal("queued cancel did not terminate the Workflow")
	}
	if r, err := f.owners.c.store.Run(t.Context(), run.ID); err != nil || r.Status != stepmodel.RunQueued {
		t.Fatal("run reported ended before its Workflow ended", r.Status, err)
	}
}

// controllerWrites returns the pinned controller's Workflows for a stopped run
// and its later waiter, renamed onto this run's Workflow and repository.
func (f *composedFixture) controllerWrites(t *testing.T, run stateport.Run) (ended, waiting *wf.Workflow) {
	t.Helper()
	raw, err := os.ReadFile(cancelledLocks)
	if err != nil {
		t.Fatal(err)
	}
	text := strings.NewReplacer("ended-heavy-stop", run.Origin.WorkflowName, `"default`, `"`+f.config.PipelineNamespace).Replace(string(raw))
	var rows []wf.Workflow
	if err = json.Unmarshal([]byte(text), &rows); err != nil {
		t.Fatal(err)
	}
	for i := range rows {
		switch rows[i].Name {
		case run.Origin.WorkflowName:
			ended = &rows[i]
		case "waiting-heavy-stop":
			waiting = &rows[i]
		}
	}
	if ended == nil || waiting == nil || waiting.Status.Synchronization == nil {
		t.Fatal("controller fixture lacks the stopped run")
	}
	waiting.Labels[nativebinding.RepositoryLabel] = strconv.FormatInt(run.RepositoryID, 10)
	return ended, waiting
}

func (f *composedFixture) nativeWorkflow(t *testing.T, run stateport.Run) *wf.Workflow {
	t.Helper()
	w, err := f.argo.ArgoprojV1alpha1().Workflows(f.config.PipelineNamespace).Get(t.Context(), run.Origin.WorkflowName, meta.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return w
}

func (f *composedFixture) updateNative(t *testing.T, w *wf.Workflow) {
	t.Helper()
	if _, err := f.argo.ArgoprojV1alpha1().Workflows(w.Namespace).Update(t.Context(), w, meta.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
}

// lockStatus reads one lock through the command line as the administrator.
func (f *composedFixture) lockStatus(t *testing.T, lock string) apicontract.LockState {
	t.Helper()
	app, out, diagnostic := f.revokeApplication(t)
	if code := app.run(t.Context(), []string{"lock", "status", "--lock", lock, "-o", "json"}); code != 0 {
		t.Fatal("lock status failed", code, diagnostic.String())
	}
	var status apicontract.SyncStatus
	if err := json.Unmarshal(out.Bytes(), &status); err != nil || len(status.Locks) != 1 {
		t.Fatal("lock status output", out.String(), err)
	}
	return status.Locks[0]
}

func lockHolderRuns(holders []apicontract.LockHolder) []string {
	runs := make([]string, len(holders))
	for i, holder := range holders {
		runs[i] = holder.RunID
	}
	return runs
}
