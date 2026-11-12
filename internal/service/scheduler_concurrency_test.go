package service

import (
	"context"
	"io"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/runlog"
	"github.com/oberthci/oberth/internal/store"
	"github.com/oberthci/oberth/internal/storetest"
	"github.com/oberthci/oberth/pkg/pipeline"
)

// concurrentJobs holds every run inside Wait so the test can observe how many
// runs reached Job creation at the same time. Job creation is the observable
// that matters: a run gated before it has a durable "running" record and no
// execution object anywhere, which is precisely the state this test guards.
type concurrentJobs struct {
	mu       sync.Mutex
	size     pipeline.Size
	created  []string
	admitted chan string
	release  chan struct{}
}

func newConcurrentJobs(size pipeline.Size, capacity int) *concurrentJobs {
	return &concurrentJobs{
		size:     size,
		admitted: make(chan string, capacity),
		release:  make(chan struct{}),
	}
}

func (jobs *concurrentJobs) CreateWorkflow(_ context.Context, request WorkflowRequest) error {
	jobs.mu.Lock()
	jobs.created = append(jobs.created, request.Run.ID)
	jobs.mu.Unlock()
	jobs.admitted <- request.Run.ID
	return nil
}

func (jobs *concurrentJobs) CreateRelease(context.Context, WorkflowRequest) error { return nil }

func (jobs *concurrentJobs) PipelineSize(context.Context, WorkflowRequest) (pipeline.Size, error) {
	return jobs.size.Effective(), nil
}

func (jobs *concurrentJobs) Wait(ctx context.Context, _ string, _ io.Writer) (JobResult, error) {
	select {
	case <-jobs.release:
	case <-ctx.Done():
		return JobResult{}, ctx.Err()
	}
	return JobResult{
		Status: model.RunPassed, Phase: "passed",
		Steps: []model.StepResult{stepResult(model.StepPassed)},
	}, nil
}

func (jobs *concurrentJobs) Delete(context.Context, string, string) error { return nil }

func (jobs *concurrentJobs) TerminalResult(context.Context, string) (JobResult, error) {
	return JobResult{}, ErrJobNotTerminal
}

type concurrencyFixture struct {
	store     *store.Store
	repo      model.Repository
	jobs      *concurrentJobs
	scheduler *Scheduler
}

func newConcurrencyFixture(t *testing.T, size pipeline.Size, maxConcurrentJobs int) *concurrencyFixture {
	t.Helper()
	ctx := context.Background()
	root := t.TempDir()
	database, err := openTestStore(t, ctx, filepath.Join(root, "oberth.sqlite"), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	upstream, err := storetest.CreateUpstream(database, ctx, model.UpstreamSpec{
		Name: "codeberg", Kind: "ssh", BaseURL: "ssh://codeberg.org/acme",
	})
	if err != nil {
		t.Fatal(err)
	}
	repository, err := database.CreateRepository(ctx, model.RepositorySpec{
		Name: "oberth", UpstreamID: upstream.ID, DefaultBranch: "main",
	})
	if err != nil {
		t.Fatal(err)
	}
	logs, err := runlog.Open(filepath.Join(root, "logs"))
	if err != nil {
		t.Fatal(err)
	}
	// The admitted channel is unbounded relative to the budget so an
	// over-admission is recorded rather than blocked into invisibility.
	jobs := newConcurrentJobs(size, maxConcurrentJobs+2)
	scheduler, err := NewScheduler(SchedulerConfig{
		Store: database, Git: &controlGit{}, Logs: logs, Jobs: jobs, ReleaseJobs: jobs,
		Auditor: database, Signals: NewSignals(),
		WorkspaceRoot: filepath.Join(root, "work")})
	if err != nil {
		t.Fatal(err)
	}
	return &concurrencyFixture{store: database, repo: repository, jobs: jobs, scheduler: scheduler}
}

// enqueue adds one run on its own branch of the same repository, so the runs
// are genuinely concurrent work rather than supersessions of each other.
func (fixture *concurrencyFixture) enqueue(t *testing.T, index int) {
	t.Helper()
	digit := string("0123456789abcdef"[index%16])
	branch := "feature/concurrent-" + digit
	if _, err := fixture.scheduler.EnqueueCI(context.Background(), CIRequest{
		EventID:    "concurrent-" + branch,
		Repository: fixture.repo,
		Branch:     branch,
		SHA:        strings.Repeat(digit, 40),
		Actor:      "agent@host",
	}); err != nil {
		t.Fatal(err)
	}
}
