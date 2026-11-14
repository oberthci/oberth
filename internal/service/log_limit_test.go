package service

import (
	"context"
	"io"
	"path/filepath"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/model"
)

type overflowingJobs struct{}

func (overflowingJobs) CreateWorkflow(context.Context, WorkflowRequest) error { return nil }
func (overflowingJobs) Stop(context.Context, string, string) error            { return nil }
func (overflowingJobs) TerminalResult(context.Context, string) (JobResult, error) {
	return JobResult{Status: model.RunPassed, Phase: "passed"}, nil
}
func (overflowingJobs) Wait(_ context.Context, _ string, destination io.Writer) (JobResult, error) {
	_, err := io.WriteString(destination, "[test/unit] "+strings.Repeat("x", 128)+"\n")
	return JobResult{Status: model.RunPassed, Phase: "passed"}, err
}

func TestRunObservationOutputPreservesArgoVerdict(t *testing.T) {
	fixture := newControlFixture(t)
	scheduler, err := NewScheduler(SchedulerConfig{
		Store: fixture.store, Git: fixture.git, Logs: fixture.logs, Jobs: overflowingJobs{},
		Auditor: fixture.store, Signals: fixture.signals,
		WorkspaceRoot: filepath.Join(fixture.root, "bounded-log-work"),
	})
	if err != nil {
		t.Fatal(err)
	}
	enqueued, err := scheduler.recordCIForTest(context.Background(), CIRequest{
		EventID: "receive-log-overflow", Repository: fixture.repo, Branch: "feature/log-overflow",
		SHA: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Actor: "agent@host",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := scheduler.ProcessNext(context.Background()); err != nil {
		t.Fatal(err)
	}
	finished, err := fixture.store.Run(context.Background(), enqueued.ID)
	if err != nil {
		t.Fatal(err)
	}
	if finished.Status != model.RunPassed || finished.Error != "" {
		t.Fatalf("overflow run = %#v", finished)
	}
}

func (j overflowingJobs) Terminate(ctx context.Context, name, id string) error {
	return j.Stop(ctx, name, id)
}
