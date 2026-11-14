package service

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/model"
)

// verdictUnreadableStore fails the run read that carries the assisted
// verdict; every other call reaches the real store.
type verdictUnreadableStore struct {
	SchedulerStore
	runID string
}

func (store verdictUnreadableStore) Run(ctx context.Context, id string) (model.Run, error) {
	if id == store.runID {
		return model.Run{}, errors.New("database is locked")
	}
	return store.SchedulerStore.Run(ctx, id)
}

// A passed run whose assisted verdict cannot be read at finalization is not
// finished silently unpublished: the read error is returned, the run stays
// unfinished for the next finalization, and nothing is published.
func TestFinalizeReturnsUnreadableAssistedVerdict(t *testing.T) {
	result := JobResult{Status: model.RunPassed, Phase: "test", Steps: []model.StepResult{{NodeID: "ci.unit", Stage: "ci", Step: "unit", Status: model.StepPassed}}}
	f := newControlFixture(t, result)
	source, err := f.scheduler.recordCIForTest(t.Context(), CIRequest{EventID: "verdict", Repository: f.repo, Branch: "feature/verdict", SHA: strings.Repeat("e", 40), Actor: "pusher"})
	if err != nil {
		t.Fatal(err)
	}
	run, err := f.store.StartRun(t.Context(), source.ID)
	if err != nil || run.ID != source.ID {
		t.Fatalf("claim = %+v %v", run, err)
	}
	real := f.scheduler.store
	f.scheduler.store = verdictUnreadableStore{SchedulerStore: real, runID: run.ID}
	err = f.scheduler.finalizeReconciledRun(t.Context(), run, result, f.scheduler.requireMutation)
	f.scheduler.store = real
	if err == nil || !strings.Contains(err.Error(), "assisted verdict") {
		t.Fatalf("finalize = %v, want the unreadable assisted verdict", err)
	}
	current, err := f.store.Run(t.Context(), run.ID)
	if err != nil || current.Status.Terminal() {
		t.Fatalf("run finished despite an unreadable verdict: %+v %v", current, err)
	}
	publications, err := f.store.PendingPublications(t.Context())
	if err != nil || len(publications) != 0 {
		t.Fatalf("publications = %+v %v", publications, err)
	}
}
