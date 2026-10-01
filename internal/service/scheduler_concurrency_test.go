package service

import (
	"context"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/runlog"
	"github.com/oberthci/oberth/internal/store"
	"github.com/oberthci/oberth/pkg/periapsis"
)

// concurrentJobs holds every run inside Wait so the test can observe how many
// runs reached Job creation at the same time. Job creation is the observable
// that matters: a run gated before it has a durable "running" record and no
// execution object anywhere, which is precisely the state this test guards.
type concurrentJobs struct {
	mu       sync.Mutex
	size     periapsis.Size
	created  []string
	admitted chan string
	release  chan struct{}
}

func newConcurrentJobs(size periapsis.Size, capacity int) *concurrentJobs {
	return &concurrentJobs{
		size:     size,
		admitted: make(chan string, capacity),
		release:  make(chan struct{}),
	}
}

func (jobs *concurrentJobs) CreateCI(_ context.Context, request JobRequest) error {
	jobs.mu.Lock()
	jobs.created = append(jobs.created, request.Run.ID)
	jobs.mu.Unlock()
	jobs.admitted <- request.Run.ID
	return nil
}

func (jobs *concurrentJobs) CreateRelease(context.Context, JobRequest) error { return nil }

func (jobs *concurrentJobs) PipelineSize(context.Context, JobRequest) (periapsis.Size, error) {
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

// TestSchedulerCreatesJobsConcurrentlyForOneRepository is the end-to-end guard
// for the defect the admission unit conversion fixes.
//
// Every run in the reported failure targeted a single repository whose
// pipeline declares the L size. The scheduler claimed each run -- durably
// marking it "running" and setting started_at -- then blocked in the admission
// gate before asking the engine to create anything. The result was several
// runs reporting "running" for many minutes with exactly one execution object
// in the cluster: a max-concurrency-of-1 server wearing a concurrency-of-3
// label.
//
// The assertion is about Job creation rather than about the gate in isolation,
// so it fails for any regression anywhere on the claim-to-creation path,
// including a new serialization point introduced outside admission.go.
//
// It is two-sided on purpose. The lower bound is the reported bug; the upper
// bound is the node budget the gate exists to enforce, which must not be
// traded away to buy the lower bound back.
func TestSchedulerCreatesJobsConcurrentlyForOneRepository(t *testing.T) {
	const maxConcurrentJobs = 3
	// Concurrency the operator's configured ceiling actually delivers per
	// declared size. maxConcurrentJobs is the ceiling; a pipeline heavier than
	// the default trades concurrency for per-run resources proportionally,
	// and one lighter than the default is still capped by the ceiling.
	sizes := []struct {
		declared periapsis.Size
		expected int
	}{
		{declared: "", expected: maxConcurrentJobs},
		{declared: periapsis.S, expected: maxConcurrentJobs},
		{declared: periapsis.M, expected: maxConcurrentJobs},
		{declared: periapsis.L, expected: 2},
		{declared: periapsis.XL, expected: 1},
	}
	for _, size := range sizes {
		t.Run(fmt.Sprintf("size=%s", size.declared.Effective()), func(t *testing.T) {
			fixture := newConcurrencyFixture(t, size.declared, maxConcurrentJobs)
			// One more run than the size may admit, so the upper bound is
			// observable rather than merely unexercised.
			for index := range size.expected + 1 {
				fixture.enqueue(t, index)
			}

			schedulerCtx, stopScheduler := context.WithCancel(context.Background())
			defer stopScheduler()
			finished := make(chan error, 1)
			go func() { finished <- fixture.scheduler.Run(schedulerCtx) }()
			defer func() {
				close(fixture.jobs.release)
				stopScheduler()
				select {
				case <-finished:
				case <-time.After(15 * time.Second):
					t.Error("scheduler did not stop")
				}
			}()

			// Lower bound: every run the size admits must reach Job creation
			// while all the others are still held inside Wait. A serializing
			// gate delivers the first and then nothing.
			deadline := time.NewTimer(15 * time.Second)
			defer deadline.Stop()
			for admitted := 1; admitted <= size.expected; admitted++ {
				select {
				case <-fixture.jobs.admitted:
				case err := <-finished:
					t.Fatalf("scheduler stopped after %d of %d concurrent submissions: %v",
						admitted-1, size.expected, err)
				case <-deadline.C:
					t.Fatalf("only %d of %d runs reached Job creation concurrently at size %q; "+
						"the rest are durably running with no execution object",
						admitted-1, size.expected, size.declared.Effective())
				}
			}

			// Upper bound: the run past the budget must wait for a slot.
			select {
			case <-fixture.jobs.admitted:
				t.Fatalf("size %q admitted more than %d concurrent runs; the node budget is not being enforced",
					size.declared.Effective(), size.expected)
			case <-time.After(250 * time.Millisecond):
			}
		})
	}
}

type concurrencyFixture struct {
	store     *store.Store
	repo      model.Repository
	jobs      *concurrentJobs
	scheduler *Scheduler
}

func newConcurrencyFixture(t *testing.T, size periapsis.Size, maxConcurrentJobs int) *concurrencyFixture {
	t.Helper()
	ctx := context.Background()
	root := t.TempDir()
	database, err := store.Open(ctx, filepath.Join(root, "oberth.sqlite"), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	upstream, err := database.CreateUpstream(ctx, model.UpstreamSpec{
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
		WorkspaceRoot: filepath.Join(root, "work"), MaxConcurrent: maxConcurrentJobs,
	})
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
