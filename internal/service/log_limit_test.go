package service

import (
	"bytes"
	"context"
	"errors"
	"io"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/runlog"
	"github.com/oberthci/oberth/internal/runprogress"
	"github.com/oberthci/oberth/pkg/periapsis"
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

func TestStepProgressLogWriterJournalsControlRecordsWithoutRetainingThem(t *testing.T) {
	logs, err := runlog.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	started := time.Now().UTC()
	event := runprogress.Event{
		Version: runprogress.Version, Burn: "test", Step: "unit", Ordinal: 0,
		Status: runprogress.StepRunning, DeclaredSize: periapsis.M, StartedAt: &started,
	}
	marker, err := runprogress.MarshalMarker(event)
	if err != nil {
		t.Fatal(err)
	}
	var retained bytes.Buffer
	writer := newStepProgressLogWriter(&retained, func(event runprogress.Event) error {
		return logs.AppendStepProgress("r-live", event)
	})
	input := append([]byte("[test/unit] command output\n"), marker...)
	input = append(input, []byte("[test/unit] still running")...)
	for _, fragment := range [][]byte{input[:7], input[7:31], input[31:]} {
		if _, err := writer.Write(fragment); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if retained.String() != "[test/unit] command output\n[test/unit] still running" || strings.Contains(retained.String(), runprogress.MarkerPrefix) {
		t.Fatalf("retained log = %q", retained.String())
	}
	events, err := logs.StepProgress("r-live")
	if err != nil || len(events) != 1 || events[0].Status != runprogress.StepRunning {
		t.Fatalf("journaled progress = %#v, %v", events, err)
	}
}

type overflowingJobs struct{}

func (overflowingJobs) CreateCI(context.Context, JobRequest) error   { return nil }
func (overflowingJobs) Delete(context.Context, string, string) error { return nil }
func (overflowingJobs) TerminalResult(context.Context, string) (JobResult, error) {
	return JobResult{}, ErrJobNotTerminal
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
		WorkspaceRoot: filepath.Join(fixture.root, "bounded-log-work"), MaxConcurrent: 1, MaxRunLogBytes: 64,
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
