package service

import (
	"context"
	"errors"
	"testing"

	"github.com/oberthci/oberth/internal/model"
)

type failedRowHistory struct {
	RunHistory
	allRows, exactRows int
}

func (history *failedRowHistory) StepResults(context.Context, string) ([]model.StepResult, error) {
	history.allRows++
	return nil, errors.New("run list must not load every native row")
}
func (history *failedRowHistory) StepResult(ctx context.Context, runID, nodeID string) (model.StepResult, error) {
	history.exactRows++
	return history.RunHistory.StepResult(ctx, runID, nodeID)
}

func TestFailedRunListReadsOnlyFailedNode(t *testing.T) {
	f := newRetentionFixture(t)
	run := f.run(t, "main", "ci", true)
	// Presentation accepts the terminal run and reads its authoritative row.
	run.FailedStep = "test.unit"
	run.Status = model.RunFailed
	history := &failedRowHistory{RunHistory: f.api.history}
	f.api.history = history
	got, err := f.api.presentationRuns(t.Context(), []model.Run{run})
	if err != nil || len(got) != 1 || got[0].FailedNode == nil || got[0].FailedNode.NodeID != run.FailedStep {
		t.Fatal(got, err)
	}
	if history.allRows != 0 || history.exactRows != 1 {
		t.Fatal("unbounded failed-row read", history.allRows, history.exactRows)
	}
	missing := run
	missing.FailedStep = "unknown"
	got, err = f.api.presentationRuns(t.Context(), []model.Run{missing})
	if err != nil || got[0].FailedNode != nil {
		t.Fatal("missing row invented", got, err)
	}
}

func TestScopedFailedNodeReadHonorsRepositoryScope(t *testing.T) {
	f := newRetentionFixture(t)
	run := f.run(t, "main", "ci", true)
	history := &failedRowHistory{RunHistory: f.api.history}
	scoped := scopedHistory{inner: history, scope: &repoScope{repos: map[int64]model.Repository{}}}
	if _, err := scoped.StepResult(t.Context(), run.ID, "test.unit"); err == nil || history.exactRows != 0 {
		t.Fatal("hidden node read reached storage", err, history.exactRows)
	}
	scoped.scope.repos[run.RepoID] = f.repo
	row, err := scoped.StepResult(t.Context(), run.ID, "test.unit")
	if err != nil || row.NodeID != "test.unit" || history.exactRows != 1 {
		t.Fatal(row, err, history.exactRows)
	}
	other := f.run(t, "other", "ci", false)
	if _, err := scoped.StepResult(t.Context(), other.ID, "test.unit"); err == nil {
		t.Fatal("node selected from another run")
	}
}
