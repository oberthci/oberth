package service

import (
	"context"
	"encoding/json"
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
	"github.com/oberthci/oberth/internal/storetest"
	"github.com/oberthci/oberth/pkg/argoworkflow"
	"github.com/oberthci/oberth/pkg/pipeline"
)

// groupEngine is an execution engine that holds every created Job until the
// test finishes it. Its documents once declared concurrency groups; since
// v0.20.0 a declared group is recorded and ignored, and the engine only
// checks that a run is never created twice.
type groupEngine struct {
	numericCPU map[string]int64
	mu         sync.Mutex
	sizes      map[string]pipeline.Size
	jobs       map[string]*groupEngineJob // Job name -> job
	byRun      map[string]*groupEngineJob // run ID -> job
	violations []string
	created    chan string
	// probe, when set, runs at the start of every TerminalResult and Delete
	// with the Job's run ID, outside the engine lock, so a test can read the
	// scheduler's admission decisions at the exact moment it inspects or
	// deletes a Workflow.
	probe func(operation, runID string)
	// deleteErrs makes Delete fail for a run ID: the Workflow survives.
	deleteErrs map[string]error
	// deleted records every accepted Delete by Workflow name, including
	// deletions of Workflows this engine never created.
	deleted []string
}

type groupEngineJob struct {
	runID  string
	result chan JobResult
	// waitErr makes Wait fail while the Workflow keeps running: a broken
	// watch or log stream, not a terminal result.
	waitErr chan error
}

func newGroupEngine() *groupEngine {
	return &groupEngine{
		numericCPU: map[string]int64{}, sizes: map[string]pipeline.Size{},
		jobs: map[string]*groupEngineJob{}, byRun: map[string]*groupEngineJob{},
		created: make(chan string, 64), deleteErrs: map[string]error{},
	}
}

// declare records the historical size of ref's document; the retired group
// argument has no effect.
func (engine *groupEngine) declare(ref, _ string, size pipeline.Size) {
	engine.mu.Lock()
	defer engine.mu.Unlock()
	engine.sizes[ref] = size
}

func (engine *groupEngine) create(request WorkflowRequest) error {
	engine.mu.Lock()
	defer engine.mu.Unlock()
	if engine.byRun[request.Run.ID] != nil {
		engine.violations = append(engine.violations, fmt.Sprintf("run %s created twice", request.Run.ID))
	}
	job := &groupEngineJob{runID: request.Run.ID, result: make(chan JobResult, 1), waitErr: make(chan error, 1)}
	engine.jobs[request.WorkflowName] = job
	engine.byRun[request.Run.ID] = job
	engine.created <- request.Run.ID
	return nil
}

func (engine *groupEngine) CreateWorkflow(_ context.Context, request WorkflowRequest) error {
	return engine.create(request)
}

func (engine *groupEngine) CreateRelease(_ context.Context, request WorkflowRequest) error {
	return engine.create(request)
}

func (engine *groupEngine) PipelineSize(_ context.Context, request WorkflowRequest) (pipeline.Size, error) {
	engine.mu.Lock()
	defer engine.mu.Unlock()
	if size, ok := engine.sizes[request.Run.Ref]; ok {
		return size, nil
	}
	return pipeline.M, nil
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
	engine.deleted = append(engine.deleted, name)
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
		Status: model.RunFailed, Phase: "failed", FailedStage: "test", FailedStep: "unit",
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
		t.Fatalf("engine invariant violated:\n%s", strings.Join(engine.violations, "\n"))
	}
}

type groupFixture struct {
	store          *store.Store
	terraform      model.Repository
	exampleService model.Repository
	engine         *groupEngine
	scheduler      *Scheduler
	control        *API
	git            *controlGit
	stop           func()
}

func newGroupFixture(t *testing.T, _ int) *groupFixture {
	t.Helper()
	ctx := context.Background()
	root := t.TempDir()
	database, err := openTestStore(t, ctx, filepath.Join(root, "oberth.sqlite"), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	upstream, err := storetest.CreateUpstream(database, ctx, model.UpstreamSpec{Name: "codeberg", Kind: "ssh", BaseURL: "ssh://codeberg.org/acme"})
	if err != nil {
		t.Fatal(err)
	}
	terraform, err := database.CreateRepository(ctx, model.RepositorySpec{Name: "terraform", UpstreamID: upstream.ID, DefaultBranch: "main"})
	if err != nil {
		t.Fatal(err)
	}
	exampleService, err := database.CreateRepository(ctx, model.RepositorySpec{Name: "example-service", UpstreamID: upstream.ID, DefaultBranch: "main"})
	if err != nil {
		t.Fatal(err)
	}
	logs, err := runlog.Open(filepath.Join(root, "logs"))
	if err != nil {
		t.Fatal(err)
	}
	engine := newGroupEngine()
	signals := NewSignals()
	git := &controlGit{}
	scheduler, err := NewScheduler(SchedulerConfig{
		Store: database, Git: git, Logs: logs, Jobs: engine, ReleaseJobs: engine,
		Auditor: database, Signals: signals, WorkspaceRoot: filepath.Join(root, "work")})
	if err != nil {
		t.Fatal(err)
	}
	control, err := NewAPI(APIConfig{
		Runs: database, History: database, Repositories: database, Issues: database,
		Promotions: database, PromotionRuns: database, Enqueues: scheduler, Logs: logs, Auditor: database,
		Signals: signals, MaximumWait: testTimeout(50 * time.Millisecond), Admission: scheduler,
		PromotionWorkspaceRoot: filepath.Join(root, "promotion-work"), RunCancels: database,
	})
	if err != nil {
		t.Fatal(err)
	}
	// These fixtures exercise admission mechanics with XL runs; XL takes a
	// size grant since #1021.
	return &groupFixture{store: database, terraform: terraform, exampleService: exampleService, engine: engine, scheduler: scheduler, control: control, git: git}
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
		case <-time.After(testStartTimeout(15 * time.Second)):
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
	deadline := time.NewTimer(testStartTimeout(15 * time.Second))
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
	deadline := time.After(testStartTimeout(15 * time.Second))
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
	case <-time.After(testTimeout(300 * time.Millisecond)):
	}
}

func callGroupRunGet(t *testing.T, control *API, id string) RunDetailResponse {
	t.Helper()
	args, err := json.Marshal(map[string]any{"id": id, "summary": false})
	if err != nil {
		t.Fatal(err)
	}
	value, err := control.CallTool(t.Context(), api.Actor{Identity: "agent@host"}, "run_get", args)
	if err != nil {
		t.Fatalf("run_get %s: %v", id, err)
	}
	return value.(RunDetailResponse)
}

// Explicit numeric declarations for scenarios that need a full-pool head.
// A size annotation alone no longer creates that resource demand.
func (engine *groupEngine) declareCPU(ref string, milli int64) {
	engine.mu.Lock()
	defer engine.mu.Unlock()
	engine.numericCPU[ref] = milli
}

func (engine *groupEngine) PipelineDemandReceipt(_ context.Context, req WorkflowRequest) (*argoworkflow.DemandReceipt, error) {
	engine.mu.Lock()
	cpu := engine.numericCPU[req.Run.Ref]
	engine.mu.Unlock()
	if cpu == 0 {
		return nil, nil
	}
	source := []byte(fmt.Sprintf(`apiVersion: argoproj.io/v1alpha1
kind: Workflow
spec:
  activeDeadlineSeconds: 3600
  entrypoint: test
  templates:
  - name: test
    container:
      image: example.invalid/runner@sha256:%s
      resources:
        requests: {cpu: %dm, memory: 1Gi}
        limits: {memory: 1Gi}
`, strings.Repeat("a", 64), cpu))
	resolved, err := argoworkflow.ResolveRunSource(source, nil, argoworkflow.RunSourceBinding{RunID: req.Run.ID, RepositoryID: req.Repository.ID, SHA: req.Run.SHA, File: ".oberth/build.yaml", Trigger: argoworkflow.RunTriggerBranch})
	if err != nil {
		return nil, err
	}
	return resolved.Receipt(argoworkflow.Policy{RunnerImagePrefixes: []string{"example.invalid/"}})
}
