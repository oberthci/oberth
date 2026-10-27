package service

import (
	"context"
	"io"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/api"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/runprogress"
	"github.com/oberthci/oberth/internal/store"
	"github.com/oberthci/oberth/pkg/argoworkflow"
	"github.com/oberthci/oberth/pkg/periapsis"
)

func kubeVirtRequest(id string, repoID int64, demand, limit argoworkflow.VMIDemand) admissionRequest {
	request := groupRequest(id, repoID, periapsis.S, "")
	request.KubeVirt = &KubeVirtResources{Demand: demand, Limit: limit}
	return request
}

func TestKubeVirtAdmissionAccountsEveryDimension(t *testing.T) {
	for _, limit := range []argoworkflow.VMIDemand{
		{VMs: 2, Cores: 64, MemoryBytes: 256 << 30},
		{VMs: 16, Cores: 4, MemoryBytes: 256 << 30},
		{VMs: 16, Cores: 64, MemoryBytes: 2 << 30},
	} {
		gate := newWeightedAdmissionForJobs(6)
		demand := argoworkflow.VMIDemand{VMs: 1, Cores: 2, MemoryBytes: 1 << 30}
		first := acquireRequestWithinBudget(t, gate.reserve(1), kubeVirtRequest("first", 1, demand, limit))
		second := acquireRequestWithinBudget(t, gate.reserve(2), kubeVirtRequest("second", 1, demand, limit))
		third := gate.reserve(3)
		t.Cleanup(third.cancel)
		acquired := make(chan func(), 1)
		go acquireRequestForTest(third, kubeVirtRequest("third", 1, demand, limit), acquired)
		awaitWaiting(t, third)
		observation := (&Scheduler{admission: gate}).AdmissionSnapshot(3)
		if !observation.KubeVirtCapacity || waitingAdmissionMessage(observation) != "KubeVirt capacity" || gate.groupParked() != 1 {
			t.Fatalf("capacity park = %#v", observation)
		}
		other := acquireRequestWithinBudget(t, gate.reserve(4), kubeVirtRequest("other-repo", 2, demand, limit))
		first()
		thirdRelease := awaitAcquired(t, acquired)
		thirdRelease()
		second()
		other()
		gate.mu.Lock()
		used := gate.kubeVirtUsedLocked(1)
		gate.mu.Unlock()
		if used != (argoworkflow.VMIDemand{}) {
			t.Fatalf("released capacity leaked: %#v", used)
		}
	}
}

func TestKubeVirtDeletionDuringCreationKeepsCapacity(t *testing.T) {
	gate := newWeightedAdmissionForJobs(6)
	unit := argoworkflow.VMIDemand{VMs: 1, Cores: 1, MemoryBytes: 1 << 30}
	holder := gate.reserve(1)
	release := acquireRequestWithinBudget(t, holder, kubeVirtRequest("holder", 1, unit, unit))
	defer release()
	waiter := gate.reserve(2)
	t.Cleanup(waiter.cancel)
	acquired := make(chan func(), 1)
	go acquireRequestForTest(waiter, kubeVirtRequest("waiter", 1, unit, unit), acquired)
	awaitWaiting(t, waiter)
	gate.releaseDeletedRun("holder")
	gate.mu.Lock()
	used := gate.kubeVirtUsedLocked(1)
	held := holder.granted && !holder.released
	gate.mu.Unlock()
	if !held || used != unit {
		t.Fatalf("in-flight creation lost capacity: held=%v used=%#v", held, used)
	}
	awaitWaiting(t, waiter)
	holder.markSubmitted()
	gate.releaseDeletedRun("holder")
	releaseSuccessor := awaitAcquired(t, acquired)
	// Completion of the old observer must not release its successor's grant.
	release()
	gate.mu.Lock()
	used = gate.kubeVirtUsedLocked(1)
	gate.mu.Unlock()
	if used != unit {
		t.Fatalf("successor lost capacity: %#v", used)
	}
	releaseSuccessor()
	gate.mu.Lock()
	used = gate.kubeVirtUsedLocked(1)
	gate.mu.Unlock()
	if used != (argoworkflow.VMIDemand{}) {
		t.Fatalf("capacity leaked: %#v", used)
	}
}

func TestKubeVirtCapacityKeepsFIFOAndDeclaredGroup(t *testing.T) {
	gate := newWeightedAdmissionForJobs(6)
	unit := argoworkflow.VMIDemand{VMs: 1, Cores: 1, MemoryBytes: 1 << 30}
	limit := argoworkflow.VMIDemand{VMs: 2, Cores: 2, MemoryBytes: 2 << 30}
	holder := acquireRequestWithinBudget(t, gate.reserve(1), kubeVirtRequest("holder", 1, unit, limit))
	older, newer := gate.reserve(2), gate.reserve(3)
	t.Cleanup(older.cancel)
	t.Cleanup(newer.cancel)
	oldAcquired, newAcquired := make(chan func(), 1), make(chan func(), 1)
	oldRequest := kubeVirtRequest("older", 1, limit, limit)
	oldRequest.Group = "release-state"
	go acquireRequestForTest(older, oldRequest, oldAcquired)
	awaitWaiting(t, older)
	// This smaller run fits, but cannot take capacity ahead of the parked run.
	go acquireRequestForTest(newer, kubeVirtRequest("newer", 1, unit, limit), newAcquired)
	awaitWaiting(t, newer)
	grouped := gate.reserve(4)
	t.Cleanup(grouped.cancel)
	groupAcquired := make(chan func(), 1)
	// A declared group still serializes ordinary and VMI-bearing runs together.
	go acquireRequestForTest(grouped, groupRequest("ordinary-release", 1, periapsis.S, "release-state"), groupAcquired)
	awaitWaiting(t, grouped)
	holder()
	releaseOld := awaitAcquired(t, oldAcquired)
	awaitWaiting(t, newer)
	awaitWaiting(t, grouped)
	releaseOld()
	awaitAcquired(t, newAcquired)()
	awaitAcquired(t, groupAcquired)()
}

type kubeVirtEngine struct {
	*groupEngine
	resources map[string]KubeVirtResources
	holdWait  bool
}

func (engine *kubeVirtEngine) PipelineKubeVirt(_ context.Context, request JobRequest) (KubeVirtResources, error) {
	return engine.resources[request.Run.Ref], nil
}

func (engine *kubeVirtEngine) Wait(ctx context.Context, name string, output io.Writer) (JobResult, error) {
	result, err := engine.groupEngine.Wait(ctx, name, output)
	if engine.holdWait && err == nil {
		<-ctx.Done()
		return JobResult{}, ctx.Err()
	}
	return result, err
}

func kubeVirtFixture(t *testing.T, refs ...string) (*groupFixture, *kubeVirtEngine) {
	t.Helper()
	fixture := newGroupFixture(t, 6)
	engine := &kubeVirtEngine{groupEngine: fixture.engine, resources: map[string]KubeVirtResources{}}
	unit := argoworkflow.VMIDemand{VMs: 1, Cores: 2, MemoryBytes: 1 << 30}
	limit := argoworkflow.VMIDemand{VMs: 2, Cores: 4, MemoryBytes: 2 << 30}
	for _, ref := range refs {
		engine.resources[ref] = KubeVirtResources{Demand: unit, Limit: limit}
	}
	fixture.scheduler.jobs, fixture.scheduler.releaseJobs = engine, engine
	return fixture, engine
}

func TestKubeVirtSchedulerConcurrentRunsAndQueueReason(t *testing.T) {
	fixture, _ := kubeVirtFixture(t, "first", "second", "third")
	first := fixture.branch(t, fixture.terraform, "first", "a")
	second := fixture.branch(t, fixture.terraform, "second", "b")
	third := fixture.branch(t, fixture.terraform, "third", "c")
	fixture.start(t)
	created := fixture.awaitCreated(t, 2)
	if !created[first.ID] || !created[second.ID] {
		t.Fatalf("concurrent runs = %v", created)
	}
	fixture.awaitAdmission(t, third, "KubeVirt capacity", func(o *model.AdmissionObservation) bool { return o != nil && o.KubeVirtCapacity })
	listed, err := fixture.control.Runs(t.Context(), api.Actor{Identity: "agent@host"}, api.RunFilter{})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, run := range listed.([]model.Run) {
		if run.ID != third.ID {
			continue
		}
		found = true
		if run.DisplayStatus != "queued" || run.DisplayReason != "KubeVirt capacity" || run.StartedAt != nil || run.ConcurrencyGroup != "" {
			t.Fatalf("parked presentation = %#v", run)
		}
	}
	if !found {
		t.Fatal("parked run missing")
	}
	fixture.engine.finish(t, first.ID)
	if created := fixture.awaitCreated(t, 1); !created[third.ID] {
		t.Fatalf("capacity successor = %v", created)
	}
	fixture.engine.finish(t, second.ID)
	fixture.engine.finish(t, third.ID)
	fixture.awaitTerminal(t, third.ID)
}

// Cron queueing is covered by app's schedule tests. Once enqueued, a nightly
// must retain its identity and queue place while ordinary branch runs saturate
// the repository's VMI budget; it must not bypass resource admission.
func TestKubeVirtScheduledRunQueuesBehindBusyBranches(t *testing.T) {
	fixture, _ := kubeVirtFixture(t, "first", "second", "third", "main")
	first := fixture.branch(t, fixture.terraform, "first", "a")
	second := fixture.branch(t, fixture.terraform, "second", "b")
	third := fixture.branch(t, fixture.terraform, "third", "c")
	fixture.start(t)
	fixture.awaitCreated(t, 2)
	fixture.awaitAdmission(t, third, "KubeVirt capacity", func(o *model.AdmissionObservation) bool { return o != nil && o.KubeVirtCapacity })

	queued, err := fixture.scheduler.EnqueueCI(t.Context(), CIRequest{
		EventID: "nightly-while-busy", Repository: fixture.terraform,
		Branch: "main", SHA: strings.Repeat("d", 40), Actor: "oberth:scheduler",
		Trigger: "schedule", ScheduleEntry: "kubevirt-nightly",
	})
	if err != nil {
		t.Fatal(err)
	}
	nightly := fixture.run(t, queued.ID)
	fixture.awaitAdmission(t, nightly, "KubeVirt capacity", func(o *model.AdmissionObservation) bool { return o != nil && o.KubeVirtCapacity })
	nightly = fixture.run(t, nightly.ID)
	if !nightly.Status.Active() || nightly.StartedAt != nil || nightly.Trigger != "schedule" || nightly.ScheduleEntry != "kubevirt-nightly" || fixture.engine.wasCreated(nightly.ID) {
		t.Fatalf("nightly lost or bypassed capacity: %#v", nightly)
	}
	if !fixture.run(t, third.ID).Status.Active() {
		t.Fatal("nightly superseded an ordinary branch run")
	}

	fixture.engine.finish(t, first.ID)
	if created := fixture.awaitCreated(t, 1); !created[third.ID] {
		t.Fatalf("nightly overtook the earlier branch waiter: %v", created)
	}
	fixture.engine.finish(t, second.ID)
	if created := fixture.awaitCreated(t, 1); !created[nightly.ID] {
		t.Fatalf("nightly did not start after capacity was released: %v", created)
	}
	fixture.engine.finish(t, third.ID)
	fixture.engine.finish(t, nightly.ID)
	// This engine finishes jobs as failed to avoid forge publication; admission
	// and retained identity are the properties under test.
	if finished := fixture.awaitTerminal(t, nightly.ID); finished.Status != model.RunFailed || finished.ScheduleEntry != "kubevirt-nightly" {
		t.Fatalf("nightly completion = %#v", finished)
	}
}

func TestKubeVirtCancelAndSupersedeReleaseBeforeObserverReturns(t *testing.T) {
	for _, cancel := range []bool{true, false} {
		t.Run(map[bool]string{true: "cancel", false: "supersede"}[cancel], func(t *testing.T) {
			fixture, engine := kubeVirtFixture(t, "holder", "waiter")
			engine.holdWait = true
			for ref, resources := range engine.resources {
				resources.Limit = resources.Demand
				engine.resources[ref] = resources
			}
			holder := fixture.branch(t, fixture.terraform, "holder", "a")
			waiter := fixture.branch(t, fixture.terraform, "waiter", "b")
			fixture.start(t)
			fixture.awaitCreated(t, 1)
			fixture.awaitAdmission(t, waiter, "KubeVirt capacity", func(o *model.AdmissionObservation) bool { return o != nil && o.KubeVirtCapacity })
			if cancel {
				mustRunCancel(t, fixture.control, cancelAgent, map[string]any{"id": holder.ID})
			} else {
				fixture.branch(t, fixture.terraform, "holder", "c")
			}
			if o := fixture.scheduler.AdmissionSnapshot(holder.QueueSequence); o != nil {
				t.Fatalf("deleted run still holds capacity: %#v", o)
			}
			if created := fixture.awaitCreated(t, 1); !created[waiter.ID] {
				t.Fatalf("capacity not released before Wait returned: %v", created)
			}
		})
	}
}

func TestKubeVirtOversizedRunFailsBeforeAdmission(t *testing.T) {
	fixture, engine := kubeVirtFixture(t, "oversized")
	resources := engine.resources["oversized"]
	resources.Demand.VMs = resources.Limit.VMs + 1
	engine.resources["oversized"] = resources
	run := fixture.branch(t, fixture.terraform, "oversized", "a")
	fixture.start(t)
	finished := fixture.awaitTerminal(t, run.ID)
	if finished.Status != model.RunFailed || finished.Phase != "infrastructure" || !strings.Contains(finished.Error, "VMI run budget exceeded") || fixture.engine.wasCreated(run.ID) {
		t.Fatalf("oversized run = %#v", finished)
	}
}

func TestKubeVirtAdoptionCountsOnceAndDrainsReducedCapacity(t *testing.T) {
	gate := newWeightedAdmissionForJobs(6)
	unit := argoworkflow.VMIDemand{VMs: 1, Cores: 2, MemoryBytes: 1 << 30}
	runs := []model.Run{{ID: "first", QueueSequence: 1, RepoID: 7}, {ID: "second", QueueSequence: 2, RepoID: 7}}
	for _, run := range runs {
		first, err := gate.adopt(run, periapsis.M, unit)
		if err != nil {
			t.Fatal(err)
		}
		again, err := gate.adopt(run, periapsis.M, unit)
		if err != nil || again != first || !again.submitted {
			t.Fatalf("repeated adoption = %p, %v", again, err)
		}
		changed := unit
		changed.Cores++
		if _, err := gate.adopt(run, periapsis.M, changed); err == nil {
			t.Fatal("changed demand accepted for the same reservation")
		}
	}
	gate.mu.Lock()
	used := gate.kubeVirtUsedLocked(7)
	gate.mu.Unlock()
	if used != (argoworkflow.VMIDemand{VMs: 2, Cores: 4, MemoryBytes: 2 << 30}) {
		t.Fatalf("adoption counted incorrectly: %#v", used)
	}
	// The grant is now smaller than the already-running total. Adoption must
	// retain both runs, and new work waits for enough of that demand to drain.
	waiting := gate.reserve(3)
	t.Cleanup(waiting.cancel)
	acquired := make(chan func(), 1)
	go acquireRequestForTest(waiting, kubeVirtRequest("third", 7, unit, unit), acquired)
	awaitWaiting(t, waiting)
	gate.releaseDeletedRun("first")
	awaitWaiting(t, waiting)
	gate.releaseDeletedRun("second")
	release := awaitAcquired(t, acquired)
	gate.releaseDeletedRun("second")
	release()
	gate.mu.Lock()
	used = gate.kubeVirtUsedLocked(7)
	gate.mu.Unlock()
	if used != (argoworkflow.VMIDemand{}) {
		t.Fatalf("adoption leaked capacity: %#v", used)
	}
}

type adoptingKubeVirtEngine struct {
	*kubeVirtEngine
	adopted chan string
}

func (engine *adoptingKubeVirtEngine) JobNamespace() string { return "workflows" }

func (engine *adoptingKubeVirtEngine) Adopt(_ context.Context, run model.Run) (periapsis.Size, bool, error) {
	engine.mu.Lock()
	defer engine.mu.Unlock()
	job := engine.jobs[run.JobName]
	return periapsis.M, job != nil && job.runID == run.ID, nil
}

func (engine *adoptingKubeVirtEngine) AdoptedKubeVirtDemand(run model.Run) (argoworkflow.VMIDemand, error) {
	return engine.resources[run.Ref].Demand, nil
}

func (engine *adoptingKubeVirtEngine) WaitResumed(ctx context.Context, name string, output io.Writer, _ []runprogress.Event, _ int64) (JobResult, error) {
	engine.adopted <- name
	return engine.Wait(ctx, name, output)
}

func TestKubeVirtRestartRestoresTwoRunsBeforeAdmittingThird(t *testing.T) {
	fixture := newRecoveryFixture(t)
	unit := argoworkflow.VMIDemand{VMs: 1, Cores: 2, MemoryBytes: 1 << 30}
	limit := argoworkflow.VMIDemand{VMs: 2, Cores: 4, MemoryBytes: 2 << 30}
	engine := &adoptingKubeVirtEngine{kubeVirtEngine: &kubeVirtEngine{
		groupEngine: newGroupEngine(), resources: map[string]KubeVirtResources{},
	}, adopted: make(chan string, 2)}
	var runs []model.Run
	for i, ref := range []string{"first", "second"} {
		engine.resources[ref] = KubeVirtResources{Demand: unit, Limit: limit}
		run := fixture.enqueueAndClaim(t, ref, strings.Repeat(string(rune('a'+i)), 40))
		run.JobName = "workflow-" + ref
		fixture.setJobName(t, run.ID, run.JobName)
		if _, err := fixture.store.MarkRunSubmitted(t.Context(), run.ID); err != nil {
			t.Fatal(err)
		}
		if err := engine.CreateCI(t.Context(), JobRequest{Run: run, JobName: run.JobName}); err != nil {
			t.Fatal(err)
		}
		<-engine.created
		runs = append(runs, run)
	}
	// Reopen durable state as a new owner; no source checkout is required to
	// rebuild the demand of the two Workflows that survived the old process.
	if err := fixture.store.Close(); err != nil {
		t.Fatal(err)
	}
	var err error
	fixture.store, err = store.Open(t.Context(), filepath.Join(fixture.root, "oberth.sqlite"), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = fixture.store.Close() })
	scheduler := fixture.scheduler(t, engine)
	scheduler.maxConcurrent = 6
	scheduler.admission = newWeightedAdmissionForJobs(6)
	group := &groupFixture{store: fixture.store, terraform: fixture.repo, engine: engine.groupEngine, scheduler: scheduler}
	engine.resources["third"] = KubeVirtResources{Demand: unit, Limit: limit}
	third := group.branch(t, fixture.repo, "third", "c")
	group.start(t)
	for range runs {
		select {
		case <-engine.adopted:
		case <-time.After(60 * time.Second):
			t.Fatal("adopted observer did not start")
		}
	}
	group.awaitAdmission(t, third, "KubeVirt capacity", func(o *model.AdmissionObservation) bool { return o != nil && o.KubeVirtCapacity })
	scheduler.admission.mu.Lock()
	used := scheduler.admission.kubeVirtUsedLocked(fixture.repo.ID)
	scheduler.admission.mu.Unlock()
	if used != limit {
		t.Fatalf("restored usage = %#v, want %#v", used, limit)
	}
	for _, run := range runs {
		current := group.run(t, run.ID)
		if current.Status != model.RunRunning || current.JobName != run.JobName || current.SupersededBy != "" {
			t.Fatalf("restart replaced run: %#v", current)
		}
	}
	engine.finish(t, runs[0].ID)
	if created := group.awaitCreated(t, 1); !created[third.ID] {
		t.Fatalf("wrong successor: %v", created)
	}
	engine.finish(t, runs[1].ID)
	engine.finish(t, third.ID)
	for _, run := range append(runs, third) {
		if got := group.awaitTerminal(t, run.ID); got.Status != model.RunFailed || got.FailedStep != "unit" {
			t.Fatalf("run did not preserve its actual test result: %#v", got)
		}
	}
	// Acquiring the entire grant synchronizes with the observer's final release.
	release := acquireRequestWithinBudget(t, scheduler.admission.reserve(1000), kubeVirtRequest("probe", fixture.repo.ID, limit, limit))
	release()
	scheduler.admission.mu.Lock()
	used = scheduler.admission.kubeVirtUsedLocked(fixture.repo.ID)
	scheduler.admission.mu.Unlock()
	if used != (argoworkflow.VMIDemand{}) {
		t.Fatalf("completion leaked restored capacity: %#v", used)
	}
	if counts, err := fixture.store.CountRuns(t.Context()); err != nil || counts.Total != 3 {
		t.Fatalf("restart created extra runs: %#v, %v", counts, err)
	}
}
