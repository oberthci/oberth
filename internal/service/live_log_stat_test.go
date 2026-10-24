package service

import (
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/api"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/runlog"
)

type countedLiveLogStore struct {
	LogStore
	source *runlog.Store
	reads  int
}

func (logs *countedLiveLogStore) LiveLogSize(id string) (int64, error) {
	return logs.source.LiveLogSize(id)
}
func (logs *countedLiveLogStore) ReadFrom(id string, offset, maximum int64) ([]byte, int64, int64, error) {
	logs.reads++
	return logs.LogStore.ReadFrom(id, offset, maximum)
}

func TestRunLogLiveUnchangedLogSkipsReadAndStillReportsCompletion(t *testing.T) {
	fixture := newControlFixture(t)
	run, err := fixture.scheduler.EnqueueCI(t.Context(), CIRequest{EventID: "live-stat", Repository: fixture.repo, Branch: "feature/live-stat", SHA: strings.Repeat("a", 40), Actor: "agent@host"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.store.ClaimNextRun(t.Context()); err != nil {
		t.Fatal(err)
	}
	file, err := fixture.logs.Create(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	service := fixture.api(t)
	logs := &countedLiveLogStore{LogStore: fixture.logs, source: fixture.logs}
	service.logs = logs
	read := func(offset int64) LiveLogResponse {
		t.Helper()
		value, err := service.RunLogLive(t.Context(), api.Actor{Identity: "agent@host"}, run.ID, offset)
		if err != nil {
			t.Fatal(err)
		}
		return value.(LiveLogResponse)
	}
	if chunk := read(0); chunk.Done || logs.reads != 0 {
		t.Fatalf("empty log: %+v reads=%d", chunk, logs.reads)
	}
	if _, err := file.WriteString("one\n"); err != nil {
		t.Fatal(err)
	}
	first := read(0)
	if first.Chunk != "one\n" || first.Offset != 4 || logs.reads != 1 {
		t.Fatalf("append: %+v reads=%d", first, logs.reads)
	}
	if idle := read(first.Offset); idle.Chunk != "" || idle.Done || logs.reads != 1 {
		t.Fatalf("idle: %+v reads=%d", idle, logs.reads)
	}
	if _, err := file.WriteString("final"); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.store.FinishRun(t.Context(), run.ID, model.RunResult{Status: model.RunPassed}); err != nil {
		t.Fatal(err)
	}
	final := read(first.Offset)
	if final.Chunk != "final" || !final.Done || logs.reads != 2 {
		t.Fatalf("terminal tail: %+v reads=%d", final, logs.reads)
	}
	if idle := read(final.Offset); !idle.Done || idle.Chunk != "" || logs.reads != 2 {
		t.Fatalf("terminal idle: %+v reads=%d", idle, logs.reads)
	}
}
