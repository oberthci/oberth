package service

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/store"
)

type submissionRereadStore struct {
	*store.Store
	reads int
}

func (s *submissionRereadStore) Run(ctx context.Context, id string) (model.Run, error) {
	s.reads++
	if s.reads == 2 {
		return model.Run{}, errors.New("submission reread refused")
	}
	return s.Store.Run(ctx, id)
}
func TestSubmissionRereadFailureFinishesStartedRun(t *testing.T) {
	f := newControlFixture(t)
	accepted, err := f.scheduler.recordCIForTest(t.Context(), CIRequest{EventID: "reread", Repository: f.repo, Branch: "main", SHA: strings.Repeat("a", 40), Actor: "owner"})
	if err != nil {
		t.Fatal(err)
	}
	run, err := f.store.StartRun(t.Context(), accepted.ID)
	if err != nil {
		t.Fatal(err)
	}
	failing := &submissionRereadStore{Store: f.store}
	f.scheduler.store = failing
	owner := f.scheduler.submissions.trackRun(run)
	defer owner.cancel()
	err = f.scheduler.submitWorkflow(t.Context(), run, f.scheduler.requireMutation, owner, func(model.Run, model.Repository, runObservation) error {
		t.Fatal("failed reread submitted a Workflow")
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	final, err := f.store.Run(t.Context(), run.ID)
	if err != nil || final.Status != model.RunFailed || !strings.Contains(final.Error, "submission reread refused") || final.WorkflowName != "" {
		t.Fatal(final, err)
	}
}
