package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/api"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/runlog"
	"github.com/oberthci/oberth/internal/store"
	"github.com/oberthci/oberth/pkg/periapsis"
)

const terraformState = "terraform-state"

// groupEngine is an execution engine whose documents declare a concurrency
// group per run ref. It holds every created Job until the test finishes it,
// and it independently checks the property the scheduler must guarantee: no
// two Jobs of one repository's group ever exist at the same time.
type groupEngine struct {
	mu         sync.Mutex
	groups     map[string]string // run ref -> declared group
	groupErrs  map[string]error  // run ref -> document refusal
	sizes      map[string]periapsis.Size
	jobs       map[string]*groupEngineJob // Job name -> job
	byRun      map[string]*groupEngineJob // run ID -> job
	executing  map[string]string          // "<repo>/<group>" -> run ID
	violations []string
	created    chan string
	// probe, when set, runs at the start of every TerminalResult and Delete
	// with the Job's run ID, outside the engine lock, so a test can read the
	// scheduler's admission decisions at the exact moment it inspects or
	// deletes a Workflow.
	probe func(operation, runID string)
	// deleteErrs makes Delete fail for a run ID: the Workflow survives.
	deleteErrs map[string]error
}

type groupEngineJob struct {
	runID  string
	key    string
	result chan JobResult
	// waitErr makes Wait fail while the Workflow keeps running: a broken
	// watch or log stream, not a terminal result.
	waitErr chan error
}

func newGroupEngine() *groupEngine {
	return &groupEngine{
		groups: map[string]string{}, groupErrs: map[string]error{}, sizes: map[string]periapsis.Size{},
		jobs: map[string]*groupEngineJob{}, byRun: map[string]*groupEngineJob{},
		executing: map[string]string{}, created: make(chan string, 64), deleteErrs: map[string]error{},
	}
}

func (engine *groupEngine) declare(ref, group string, size periapsis.Size) {
	engine.mu.Lock()
	defer engine.mu.Unlock()
	engine.groups[ref] = group
	engine.sizes[ref] = size
}

func (engine *groupEngine) create(request JobRequest) error {
	engine.mu.Lock()
	defer engine.mu.Unlock()
	key := ""
	if request.ConcurrencyGroup != "" {
		key = fmt.Sprintf("%d/%s", request.Run.RepoID, request.ConcurrencyGroup)
		if holder, busy := engine.executing[key]; busy {
			engine.violations = append(engine.violations,
				fmt.Sprintf("run %s created while run %s holds %s", request.Run.ID, holder, key))
		}
		engine.executing[key] = request.Run.ID
	}
	if request.ConcurrencyGroup != engine.groups[request.Run.Ref] {
		engine.violations = append(engine.violations,
			fmt.Sprintf("run %s submitted with group %q, document declares %q", request.Run.ID, request.ConcurrencyGroup, engine.groups[request.Run.Ref]))
	}
	job := &groupEngineJob{runID: request.Run.ID, key: key, result: make(chan JobResult, 1), waitErr: make(chan error, 1)}
	engine.jobs[request.JobName] = job
	engine.byRun[request.Run.ID] = job
	engine.created <- request.Run.ID
	return nil
}

func (engine *groupEngine) CreateCI(_ context.Context, request JobRequest) error {
	return engine.create(request)
}

func (engine *groupEngine) CreateRelease(_ context.Context, request JobRequest) error {
	return engine.create(request)
}

func (engine *groupEngine) PipelineSize(_ context.Context, request JobRequest) (periapsis.Size, error) {
	engine.mu.Lock()
	defer engine.mu.Unlock()
	if size, ok := engine.sizes[request.Run.Ref]; ok {
		return size, nil
	}
	return periapsis.M, nil
}

func (engine *groupEngine) PipelineConcurrencyGroup(_ context.Context, request JobRequest) (string, error) {
	engine.mu.Lock()
	defer engine.mu.Unlock()
	if err := engine.groupErrs[request.Run.Ref]; err != nil {
		return "", err
	}
	return engine.groups[request.Run.Ref], nil
}

func (engine *groupEngine) Wait(ctx context.Context, name string, _ io.Writer) (JobResult, error) {
	engine.mu.Lock()
	job := engine.jobs[name]
	engine.mu.Unlock()
	if job == nil {
		return JobResult{}, fmt.Errorf("no Job %s", name)
	}
	select {
	case result := <-job.result:
		engine.mu.Lock()
		if job.key != "" && engine.executing[job.key] == job.runID {
			delete(engine.executing, job.key)
		}
		engine.mu.Unlock()
		return result, nil
	case err := <-job.waitErr:
		// The observation failed; the Workflow itself is still running and
		// keeps holding its group until it is deleted.
		return JobResult{}, err
	case <-ctx.Done():
		return JobResult{}, ctx.Err()
	}
}

// Delete ends a created Job the way a Workflow deletion does: the Job stops
// holding its group the moment the deletion is accepted, and a Wait still
// observing it returns. The scheduler does not always Wait on a Job it deletes:
// a run superseded between Job creation and its post-creation re-read is
// deleted and released without one, so the deletion itself must end the hold.
func (engine *groupEngine) Delete(_ context.Context, name, _ string) error {
	engine.runProbe("delete", name)
	engine.mu.Lock()
	job := engine.jobs[name]
	if job != nil && engine.deleteErrs[job.runID] != nil {
		err := engine.deleteErrs[job.runID]
		engine.mu.Unlock()
		return err
	}
	if job != nil && job.key != "" && engine.executing[job.key] == job.runID {
		delete(engine.executing, job.key)
	}
	engine.mu.Unlock()
	if job != nil {
		select {
		case job.result <- JobResult{Status: model.RunInterrupted, Phase: "interrupted", Error: "deleted"}:
		default:
		}
	}
	return nil
}

// TerminalResult never finds a terminal Workflow: every Job in this engine
// ends only through finish or Delete.
func (engine *groupEngine) TerminalResult(_ context.Context, name string) (JobResult, error) {
	engine.runProbe("terminal", name)
	return JobResult{}, ErrJobNotTerminal
}

func (engine *groupEngine) runProbe(operation, name string) {
	engine.mu.Lock()
	probe, job := engine.probe, engine.jobs[name]
	engine.mu.Unlock()
	if probe != nil && job != nil {
		probe(operation, job.runID)
	}
}

// failWait makes a created run's Wait return err while its Workflow runs on.
func (engine *groupEngine) failWait(t *testing.T, runID string, err error) {
	t.Helper()
	engine.mu.Lock()
	job := engine.byRun[runID]
	engine.mu.Unlock()
	if job == nil {
		t.Fatalf("run %s has no Job whose Wait could fail", runID)
	}
	job.waitErr <- err
}

// finish completes a created run's Job with a failed result, which keeps the
// scenario off the forge-publication path.
func (engine *groupEngine) finish(t *testing.T, runID string) {
	t.Helper()
	engine.mu.Lock()
	job := engine.byRun[runID]
	engine.mu.Unlock()
	if job == nil {
		t.Fatalf("run %s has no Job to finish", runID)
	}
	job.result <- JobResult{
		Status: model.RunFailed, Phase: "failed", FailedBurn: "test", FailedStep: "unit",
		Steps: []model.StepResult{stepResult(model.StepFailed)},
	}
}

func (engine *groupEngine) wasCreated(runID string) bool {
	engine.mu.Lock()
	defer engine.mu.Unlock()
	return engine.byRun[runID] != nil
}

func (engine *groupEngine) assertNoViolations(t *testing.T) {
	t.Helper()
	engine.mu.Lock()
	defer engine.mu.Unlock()
	if len(engine.violations) != 0 {
		t.Fatalf("concurrency group violated:\n%s", strings.Join(engine.violations, "\n"))
	}
}

type groupFixture struct {
	store     *store.Store
	terraform model.Repository
	beacon    model.Repository
	engine    *groupEngine
	scheduler *Scheduler
	control   *API
	stop      func()
}

func newGroupFixture(t *testing.T, maxConcurrent int) *groupFixture {
	t.Helper()
	ctx := context.Background()
	root := t.TempDir()
	database, err := store.Open(ctx, filepath.Join(root, "oberth.sqlite"), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	upstream, err := database.CreateUpstream(ctx, model.UpstreamSpec{Name: "codeberg", Kind: "ssh", BaseURL: "ssh://codeberg.org/acme"})
	if err != nil {
		t.Fatal(err)
	}
	terraform, err := database.CreateRepository(ctx, model.RepositorySpec{Name: "terraform", UpstreamID: upstream.ID, DefaultBranch: "main"})
	if err != nil {
		t.Fatal(err)
	}
	beacon, err := database.CreateRepository(ctx, model.RepositorySpec{Name: "beacon", UpstreamID: upstream.ID, DefaultBranch: "main"})
	if err != nil {
		t.Fatal(err)
	}
	logs, err := runlog.Open(filepath.Join(root, "logs"))
	if err != nil {
		t.Fatal(err)
	}
	engine := newGroupEngine()
	signals := NewSignals()
	scheduler, err := NewScheduler(SchedulerConfig{
		Store: database, Git: &controlGit{}, Logs: logs, Jobs: engine, ReleaseJobs: engine,
		Auditor: database, Signals: signals, WorkspaceRoot: filepath.Join(root, "work"), MaxConcurrent: maxConcurrent,
	})
	if err != nil {
		t.Fatal(err)
	}
	control, err := NewAPI(APIConfig{
		Runs: database, History: database, Repositories: database, Issues: database,
		Promotions: database, PromotionRuns: database, Enqueues: scheduler, Logs: logs, Auditor: database,
		Signals: signals, MaximumWait: 50 * time.Millisecond, Admission: scheduler,
		PromotionWorkspaceRoot: filepath.Join(root, "promotion-work"),
	})
	if err != nil {
		t.Fatal(err)
	}
	return &groupFixture{store: database, terraform: terraform, beacon: beacon, engine: engine, scheduler: scheduler, control: control}
}

func (fixture *groupFixture) start(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	finished := make(chan error, 1)
	go func() { finished <- fixture.scheduler.Run(ctx) }()
	fixture.stop = func() {
		cancel()
		select {
		case err := <-finished:
			if err != nil {
				t.Errorf("scheduler stopped with %v", err)
			}
		case <-time.After(15 * time.Second):
			t.Error("scheduler did not stop")
		}
	}
	t.Cleanup(fixture.stop)
}

func (fixture *groupFixture) branch(t *testing.T, repository model.Repository, branch, shaDigit string) model.Run {
	t.Helper()
	enqueued, err := fixture.scheduler.EnqueueCI(context.Background(), CIRequest{
		EventID: "push-" + repository.Name + "-" + branch + "-" + shaDigit, Repository: repository,
		Branch: branch, SHA: strings.Repeat(shaDigit, 40), Actor: "agent@host",
	})
	if err != nil {
		t.Fatal(err)
	}
	return fixture.run(t, enqueued.ID)
}

func (fixture *groupFixture) tag(t *testing.T, repository model.Repository, tag, objectDigit, commitDigit string) model.Run {
	t.Helper()
	enqueued, err := fixture.scheduler.AdmitRelease(context.Background(), ReleaseRequest{
		EventID: "tag-" + repository.Name + "-" + tag, Repository: repository, Tag: tag,
		ObjectSHA: strings.Repeat(objectDigit, 40), CommitSHA: strings.Repeat(commitDigit, 40), Actor: "agent@host",
	})
	if err != nil {
		t.Fatal(err)
	}
	return fixture.run(t, enqueued.ID)
}

func (fixture *groupFixture) run(t *testing.T, id string) model.Run {
	t.Helper()
	run, err := fixture.store.Run(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return run
}

// awaitTerminal waits for the durable run notification rather than assuming
// that admission of its successor means finalization has also completed.
func (fixture *groupFixture) awaitTerminal(t *testing.T, id string) model.Run {
	t.Helper()
	deadline := time.NewTimer(15 * time.Second)
	defer deadline.Stop()
	for {
		changed := fixture.scheduler.signals.Run(id)
		run := fixture.run(t, id)
		if run.Status.Terminal() {
			return run
		}
		select {
		case <-changed:
		case <-deadline.C:
			t.Fatalf("run %s did not reach terminal state; last state: %#v", id, run)
		}
	}
}

// awaitCreated waits for the next Job creations and returns them as a set.
func (fixture *groupFixture) awaitCreated(t *testing.T, count int) map[string]bool {
	t.Helper()
	created := map[string]bool{}
	deadline := time.After(15 * time.Second)
	for len(created) < count {
		select {
		case runID := <-fixture.engine.created:
			created[runID] = true
		case <-deadline:
			t.Fatalf("only %d of %d expected Job creations happened: %v", len(created), count, created)
		}
	}
	return created
}

// assertNothingCreated proves no further Job was created for a settle window.
func (fixture *groupFixture) assertNothingCreated(t *testing.T) {
	t.Helper()
	select {
	case runID := <-fixture.engine.created:
		t.Fatalf("run %s reached Job creation while it should have been waiting", runID)
	case <-time.After(300 * time.Millisecond):
	}
}

// awaitParked waits until the run holds a group-waiting reservation.
func (fixture *groupFixture) awaitParked(t *testing.T, run model.Run, holder string) *model.AdmissionObservation {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		snapshot := fixture.scheduler.AdmissionSnapshot(run.QueueSequence)
		if snapshot != nil && snapshot.State == "waiting" && snapshot.GroupHolder == holder && holder != "" {
			return snapshot
		}
		if time.Now().After(deadline) {
			t.Fatalf("run %s never parked on its group behind %s: %#v", run.ID, holder, snapshot)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// The #658 acceptance scenario: two branch pushes and an apply tag of one
// repository's group never execute concurrently and run in claim order, while
// an unrelated repository's run is admitted during the group's run.
func TestSchedulerSerializesAConcurrencyGroupAcrossTriggersWhileOtherRepositoriesRun(t *testing.T) {
	fixture := newGroupFixture(t, 3)
	fixture.engine.declare("fix-a", terraformState, periapsis.M)
	fixture.engine.declare("fix-b", terraformState, periapsis.M)
	fixture.engine.declare("apply-1", terraformState, periapsis.M)
	fixture.engine.declare("feature-x", "", periapsis.M)
	planA := fixture.branch(t, fixture.terraform, "fix-a", "a")
	planB := fixture.branch(t, fixture.terraform, "fix-b", "b")
	apply := fixture.tag(t, fixture.terraform, "apply-1", "c", "d")
	other := fixture.branch(t, fixture.beacon, "feature-x", "e")
	fixture.start(t)

	created := fixture.awaitCreated(t, 2)
	if !created[planA.ID] || !created[other.ID] {
		t.Fatalf("created %v; want the oldest group run and the other repository's run together", created)
	}
	fixture.assertNothingCreated(t)
	fixture.awaitParked(t, planB, planA.ID)
	fixture.awaitParked(t, apply, planA.ID)

	// The group is recorded on the waiting runs and projected by the tools.
	for _, waiting := range []model.Run{planB, apply} {
		stored := fixture.run(t, waiting.ID)
		if stored.Status != model.RunRunning || stored.ConcurrencyGroup != terraformState {
			t.Fatalf("waiting run %s = status %s group %q", stored.ID, stored.Status, stored.ConcurrencyGroup)
		}
	}
	status := callGroupStatus(t, fixture.control, "terraform", planB.SHA)
	if status.ConcurrencyGroup != terraformState || status.Scheduling == nil || status.Scheduling.Admission == nil ||
		status.Scheduling.Admission.GroupHolder != planA.ID ||
		!strings.Contains(status.Scheduling.Message, planA.ID) || !strings.Contains(status.Scheduling.Message, terraformState) {
		t.Fatalf("status of a group-waiting run = %s", renderStatus(t, status))
	}
	detail := callGroupRunGet(t, fixture.control, apply.ID)
	if detail.Run.ConcurrencyGroup != terraformState || detail.Scheduling == nil || detail.Scheduling.Admission.Group != terraformState {
		t.Fatalf("run_get of the waiting apply = %#v", detail)
	}
	if other := callGroupStatus(t, fixture.control, "beacon", other.SHA); other.ConcurrencyGroup != "" {
		t.Fatalf("an ungrouped run reports group %q", other.ConcurrencyGroup)
	}

	fixture.engine.finish(t, planA.ID)
	if next := fixture.awaitCreated(t, 1); !next[planB.ID] {
		t.Fatalf("after the first plan the group admitted %v; want the older branch run %s", next, planB.ID)
	}
	fixture.assertNothingCreated(t)
	fixture.engine.finish(t, planB.ID)
	if next := fixture.awaitCreated(t, 1); !next[apply.ID] {
		t.Fatalf("after both plans the group admitted %v; want the apply %s", next, apply.ID)
	}
	fixture.engine.finish(t, apply.ID)
	fixture.engine.finish(t, other.ID)
	fixture.engine.assertNoViolations(t)
}

// Parked group runs hold neither weight nor a dispatch slot. With a ceiling
// of two, a repository with three queued runs in one group would otherwise
// occupy both slots and stall every other repository: the whole-host
// reservation the group replaces. The mirrored run ceiling still holds.
func TestSchedulerParkedGroupRunsDoNotHoldDispatchSlots(t *testing.T) {
	fixture := newGroupFixture(t, 2)
	for _, ref := range []string{"fix-a", "fix-b", "fix-c"} {
		fixture.engine.declare(ref, terraformState, periapsis.M)
	}
	planA := fixture.branch(t, fixture.terraform, "fix-a", "a")
	planB := fixture.branch(t, fixture.terraform, "fix-b", "b")
	planC := fixture.branch(t, fixture.terraform, "fix-c", "c")
	otherX := fixture.branch(t, fixture.beacon, "feature-x", "d")
	otherY := fixture.branch(t, fixture.beacon, "feature-y", "e")
	fixture.start(t)

	created := fixture.awaitCreated(t, 2)
	if !created[planA.ID] || !created[otherX.ID] {
		t.Fatalf("created %v; want the group holder and the other repository's first run", created)
	}
	// Upper bound: two runs execute, the ceiling; the rest wait.
	fixture.assertNothingCreated(t)
	fixture.awaitParked(t, planB, planA.ID)
	fixture.awaitParked(t, planC, planA.ID)

	fixture.engine.finish(t, otherX.ID)
	if next := fixture.awaitCreated(t, 1); !next[otherY.ID] {
		t.Fatalf("a freed slot went to %v; want the other repository's next run while the group is held", next)
	}
	fixture.engine.finish(t, planA.ID)
	if next := fixture.awaitCreated(t, 1); !next[planB.ID] {
		t.Fatalf("group handoff admitted %v; want %s", next, planB.ID)
	}
	fixture.engine.finish(t, otherY.ID)
	fixture.engine.finish(t, planB.ID)
	if next := fixture.awaitCreated(t, 1); !next[planC.ID] {
		t.Fatalf("group handoff admitted %v; want %s", next, planC.ID)
	}
	fixture.engine.finish(t, planC.ID)
	fixture.engine.assertNoViolations(t)
}

// A newer push to the same branch still supersedes an older run, whether the
// older run is parked on the group or holding it, and the superseded run never
// reaches Job creation (parked) or frees the group for the next run (holder).
func TestSchedulerSupersedesSameBranchRunsInsideAConcurrencyGroup(t *testing.T) {
	fixture := newGroupFixture(t, 3)
	fixture.engine.declare("fix-a", terraformState, periapsis.M)
	fixture.engine.declare("fix-b", terraformState, periapsis.M)
	holder := fixture.branch(t, fixture.terraform, "fix-a", "a")
	fixture.start(t)
	if created := fixture.awaitCreated(t, 1); !created[holder.ID] {
		t.Fatalf("created %v; want %s", created, holder.ID)
	}

	older := fixture.branch(t, fixture.terraform, "fix-b", "b")
	fixture.awaitParked(t, older, holder.ID)
	newer := fixture.branch(t, fixture.terraform, "fix-b", "c")
	if superseded := fixture.run(t, older.ID); superseded.Status != model.RunInterrupted || superseded.SupersededBy != newer.ID {
		t.Fatalf("the parked older run was not superseded: %#v", superseded)
	}
	fixture.assertNothingCreated(t)

	fixture.engine.finish(t, holder.ID)
	if next := fixture.awaitCreated(t, 1); !next[newer.ID] {
		t.Fatalf("after the holder the group admitted %v; want the newest push %s", next, newer.ID)
	}
	if fixture.engine.wasCreated(older.ID) {
		t.Fatal("the superseded parked run reached Job creation")
	}

	// Supersede the running holder: its Job is deleted, its grant released,
	// and the group passes to the next run without overlap.
	newest := fixture.branch(t, fixture.terraform, "fix-b", "d")
	if next := fixture.awaitCreated(t, 1); !next[newest.ID] {
		t.Fatalf("after superseding the holder the group admitted %v; want %s", next, newest.ID)
	}
	if superseded := fixture.run(t, newer.ID); superseded.Status != model.RunInterrupted || superseded.SupersededBy != newest.ID {
		t.Fatalf("the running holder was not superseded: %#v", superseded)
	}
	fixture.engine.finish(t, newest.ID)
	fixture.engine.assertNoViolations(t)
}

// A Wait that fails is not a Workflow that ended. The group stays with the run
// until the scheduler has proved its Workflow terminal or had it deleted; only
// then may the next run of the group be admitted. The probe reads the gate's
// decision at the exact moment the scheduler inspects and deletes the old
// Workflow, so the ordering is asserted, not inferred from timing.
func TestSchedulerHoldsTheGroupUntilAFailedWaitsWorkflowIsDeleted(t *testing.T) {
	fixture := newGroupFixture(t, 3)
	fixture.engine.declare("fix-a", terraformState, periapsis.M)
	fixture.engine.declare("fix-b", terraformState, periapsis.M)
	holder := fixture.branch(t, fixture.terraform, "fix-a", "a")
	next := fixture.branch(t, fixture.terraform, "fix-b", "b")
	fixture.start(t)
	if created := fixture.awaitCreated(t, 1); !created[holder.ID] {
		t.Fatalf("created %v; want %s", created, holder.ID)
	}
	fixture.awaitParked(t, next, holder.ID)

	var observedMu sync.Mutex
	observed := map[string]string{}
	fixture.engine.mu.Lock()
	fixture.engine.probe = func(operation, runID string) {
		if runID != holder.ID {
			return
		}
		state := "released"
		if snapshot := fixture.scheduler.AdmissionSnapshot(next.QueueSequence); snapshot != nil {
			state = snapshot.State
		}
		observedMu.Lock()
		observed[operation] = state
		observedMu.Unlock()
	}
	fixture.engine.mu.Unlock()

	fixture.engine.failWait(t, holder.ID, errors.New("watch stream reset"))
	if created := fixture.awaitCreated(t, 1); !created[next.ID] {
		t.Fatalf("after the failed holder the group admitted %v; want %s", created, next.ID)
	}
	observedMu.Lock()
	terminal, deleted := observed["terminal"], observed["delete"]
	observedMu.Unlock()
	if terminal != "waiting" || deleted != "waiting" {
		t.Fatalf("the next group run was %q while the scheduler read the old Workflow and %q while it deleted it; "+
			"want it still waiting on the group both times", terminal, deleted)
	}
	failed := fixture.awaitTerminal(t, holder.ID)
	if failed.Status != model.RunFailed || failed.Phase != "job" || failed.Error != "watch stream reset" {
		t.Fatalf("holder whose Wait failed = %#v", failed)
	}
	fixture.engine.finish(t, next.ID)
	fixture.engine.assertNoViolations(t)
}

// When the Workflow of a failed Wait cannot be deleted either, the scheduler
// stops and leaves the run owned for the restart pass. The group must stay
// with that run for the rest of the process: handing it on would start the
// next member beside a Workflow that may still be alive.
func TestSchedulerKeepsTheGroupWhenAFailedWaitsWorkflowCannotBeDeleted(t *testing.T) {
	fixture := newGroupFixture(t, 3)
	fixture.engine.declare("fix-a", terraformState, periapsis.M)
	fixture.engine.declare("fix-b", terraformState, periapsis.M)
	fixture.branch(t, fixture.terraform, "fix-a", "a")
	fixture.branch(t, fixture.terraform, "fix-b", "b")
	ctx := context.Background()
	holder, err := fixture.store.ClaimNextRun(ctx)
	if err != nil {
		t.Fatal(err)
	}
	next, err := fixture.store.ClaimNextRun(ctx)
	if err != nil {
		t.Fatal(err)
	}

	holderDone := make(chan error, 1)
	go func() { holderDone <- fixture.scheduler.executeAndCleanup(ctx, holder) }()
	if created := fixture.awaitCreated(t, 1); !created[holder.ID] {
		t.Fatalf("created %v; want %s", created, holder.ID)
	}
	memberCtx, cancelMember := context.WithCancel(ctx)
	memberDone := make(chan error, 1)
	go func() { memberDone <- fixture.scheduler.executeAndCleanup(memberCtx, next) }()
	fixture.awaitParked(t, next, holder.ID)

	fixture.engine.mu.Lock()
	fixture.engine.deleteErrs[holder.ID] = errors.New("API server unavailable")
	fixture.engine.mu.Unlock()
	fixture.engine.failWait(t, holder.ID, errors.New("watch stream reset"))
	select {
	case err := <-holderDone:
		if err == nil || !strings.Contains(err.Error(), "delete Workflow") {
			t.Fatalf("holder whose Workflow could not be deleted returned %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("the holder's execution never returned")
	}
	// The holder's execution, including its deferred release and cleanup,
	// has fully returned: the gate's answer for the next member is final.
	snapshot := fixture.scheduler.AdmissionSnapshot(next.QueueSequence)
	if snapshot == nil || snapshot.State != "waiting" || snapshot.GroupHolder != holder.ID {
		t.Fatalf("next member after the holder stopped undeletable = %#v; want it still waiting on %s", snapshot, holder.ID)
	}
	if owned := fixture.run(t, holder.ID); owned.Status != model.RunRunning {
		t.Fatalf("holder = %s; want it still owned for the restart pass", owned.Status)
	}

	cancelMember()
	select {
	case <-memberDone:
	case <-time.After(15 * time.Second):
		t.Fatal("the parked member never returned after its context ended")
	}
	if fixture.engine.wasCreated(next.ID) {
		t.Fatal("the next member reached Job creation beside an undeletable Workflow")
	}
}

// Promotion CI of the same repository reads the same build document and waits
// for the group exactly like a branch run: a merged-tree run cannot overlap a
// running plan either.
func TestSchedulerPromotionCIWaitsForItsConcurrencyGroup(t *testing.T) {
	fixture := newGroupFixture(t, 3)
	fixture.engine.declare("fix-a", terraformState, periapsis.M)
	fixture.engine.declare("promotion/main", terraformState, periapsis.M)
	holder := fixture.branch(t, fixture.terraform, "fix-a", "a")
	fixture.start(t)
	if created := fixture.awaitCreated(t, 1); !created[holder.ID] {
		t.Fatalf("created %v; want %s", created, holder.ID)
	}

	result := strings.Repeat("b", 40)
	base := strings.Repeat("c", 40)
	enqueued, _, err := fixture.store.EnqueuePromotionRun(context.Background(), model.RunSpec{
		RepoID: fixture.terraform.ID, RefKind: model.RefBranch, Ref: "promotion/main", Trigger: "promotion",
		SHA: result, TestedSHA: result, BaseSHA: base, Actor: "agent@host",
	}, model.PromotionSpec{
		RepoID: fixture.terraform.ID, SourceBranch: "fix-b", SourceSHA: strings.Repeat("d", 40),
		TargetRef: "main", PreviousSHA: base, ResultSHA: result, Actor: "agent@host",
	})
	if err != nil {
		t.Fatal(err)
	}
	fixture.scheduler.NotifyQueue()
	promotion := fixture.run(t, enqueued.ID)
	snapshot := fixture.awaitParked(t, promotion, holder.ID)
	if snapshot.Group != terraformState {
		t.Fatalf("promotion CI waits on %#v", snapshot)
	}
	fixture.assertNothingCreated(t)

	fixture.engine.finish(t, holder.ID)
	if next := fixture.awaitCreated(t, 1); !next[promotion.ID] {
		t.Fatalf("after the holder the group admitted %v; want the promotion CI run %s", next, promotion.ID)
	}
	if stored := fixture.run(t, promotion.ID); stored.ConcurrencyGroup != terraformState {
		t.Fatalf("promotion run records group %q", stored.ConcurrencyGroup)
	}
	fixture.engine.finish(t, promotion.ID)
	fixture.engine.assertNoViolations(t)
}

// A malformed group is an admission error for that run alone: it fails before
// any Job exists, records no group, and never blocks the repository's later runs.
func TestSchedulerFailsARunWhoseConcurrencyGroupIsMalformed(t *testing.T) {
	fixture := newGroupFixture(t, 3)
	fixture.engine.mu.Lock()
	fixture.engine.groupErrs["fix-bad"] = errors.New(`argoworkflow: annotation oberth.ci/concurrency-group declares invalid concurrency group "Terraform_State"`)
	fixture.engine.mu.Unlock()
	fixture.engine.declare("fix-good", terraformState, periapsis.M)
	bad := fixture.branch(t, fixture.terraform, "fix-bad", "a")
	good := fixture.branch(t, fixture.terraform, "fix-good", "b")
	fixture.start(t)
	if created := fixture.awaitCreated(t, 1); !created[good.ID] {
		t.Fatalf("created %v; want only the well-formed run %s", created, good.ID)
	}
	deadline := time.Now().Add(15 * time.Second)
	for {
		failed := fixture.run(t, bad.ID)
		if failed.Status == model.RunFailed {
			if failed.Phase != "infrastructure" || !strings.Contains(failed.Error, "read pipeline concurrency group") ||
				!strings.Contains(failed.Error, "invalid concurrency group") || failed.ConcurrencyGroup != "" {
				t.Fatalf("malformed-group run = %#v", failed)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("malformed-group run never failed: %#v", failed)
		}
		time.Sleep(5 * time.Millisecond)
	}
	if fixture.engine.wasCreated(bad.ID) {
		t.Fatal("a run with a malformed group reached Job creation")
	}
	fixture.engine.finish(t, good.ID)
	fixture.engine.assertNoViolations(t)
}

func callGroupStatus(t *testing.T, control *API, repo, ref string) StatusResponse {
	t.Helper()
	args, err := json.Marshal(map[string]string{"repo": repo, "ref": ref})
	if err != nil {
		t.Fatal(err)
	}
	value, err := control.CallTool(t.Context(), api.Actor{Identity: "agent@host"}, "status", args)
	if err != nil {
		t.Fatalf("status %s@%s: %v", repo, ref, err)
	}
	return value.(StatusResponse)
}

func callGroupRunGet(t *testing.T, control *API, id string) RunDetailResponse {
	t.Helper()
	args, err := json.Marshal(map[string]string{"id": id})
	if err != nil {
		t.Fatal(err)
	}
	value, err := control.CallTool(t.Context(), api.Actor{Identity: "agent@host"}, "run_get", args)
	if err != nil {
		t.Fatalf("run_get %s: %v", id, err)
	}
	return value.(RunDetailResponse)
}

func renderStatus(t *testing.T, status StatusResponse) string {
	t.Helper()
	encoded, err := json.Marshal(status)
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}
