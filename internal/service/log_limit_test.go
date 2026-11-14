package service

import (
	"bytes"
	"context"
	"errors"
	"io"
	"path/filepath"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/model"
)

func TestRunLogWriterRetainsExactBoundAndFailsClosed(t *testing.T) {
	t.Parallel()
	var retained bytes.Buffer
	writer := newRunLogWriter(&retained, 5)
	if written, err := writer.Write([]byte("1234")); err != nil || written != 4 {
		t.Fatalf("first write = %d, %v", written, err)
	}
	if written, err := writer.Write([]byte("567")); !errors.Is(err, errRunLogLimit) || written != 1 {
		t.Fatalf("limited write = %d, %v", written, err)
	}
	if written, err := writer.Write([]byte("8")); !errors.Is(err, errRunLogLimit) || written != 0 {
		t.Fatalf("post-limit write = %d, %v", written, err)
	}
	if retained.String() != "12345" {
		t.Fatalf("retained log = %q", retained.String())
	}
}

func TestRunLogProjectionKeepsAllBytesPlain(t *testing.T) {
	var retained bytes.Buffer
	writer := newStepProgressLogWriter(&retained)
	input := "command output\n@@oberth-step-progress@@ invalid json " + strings.Repeat("x", 20000) + "\nunterminated"
	for _, fragment := range []string{input[:7], input[7:31], input[31:]} {
		if _, err := writer.Write([]byte(fragment)); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if retained.String() != input {
		t.Fatal("plain log bytes changed")
	}
	if _, err := writer.Write([]byte("closed")); err == nil {
		t.Fatal("write after close succeeded")
	}
}

type overflowingJobs struct{}

func (overflowingJobs) CreateWorkflow(context.Context, WorkflowRequest) error { return nil }
func (overflowingJobs) Stop(context.Context, string, string) error            { return nil }
func (overflowingJobs) TerminalResult(context.Context, string) (JobResult, error) {
	return JobResult{Status: model.RunFailed, Phase: "job", Error: errRunLogLimit.Error()}, nil
}
func (overflowingJobs) Wait(_ context.Context, _ string, destination io.Writer) (JobResult, error) {
	_, err := io.WriteString(destination, "[test/unit] "+strings.Repeat("x", 128)+"\n")
	return JobResult{Status: model.RunPassed, Phase: "passed"}, err
}

func TestRunLogOverflowMakesRunFailClearly(t *testing.T) {
	fixture := newControlFixture(t)
	scheduler, err := NewScheduler(SchedulerConfig{
		Store: fixture.store, Git: fixture.git, Logs: fixture.logs, Jobs: overflowingJobs{},
		Auditor: fixture.store, Signals: fixture.signals,
		WorkspaceRoot: filepath.Join(fixture.root, "bounded-log-work"), MaxRunLogBytes: 64,
	})
	if err != nil {
		t.Fatal(err)
	}
	enqueued, err := scheduler.EnqueueCI(context.Background(), CIRequest{
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
	if finished.Status != model.RunFailed || finished.Phase != "job" || !strings.Contains(finished.Error, errRunLogLimit.Error()) {
		t.Fatalf("overflow run = %#v", finished)
	}
}

func (j overflowingJobs) Terminate(ctx context.Context, name, id string) error {
	return j.Stop(ctx, name, id)
}
