package service

import (
	"errors"
	"testing"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/store"
)

func TestScopedNativeStepQueryChecksRunBeforeNode(t *testing.T) {
	f := newRetentionFixture(t)
	run := f.run(t, "main", "ci", true)
	history := &selectedNodeHistory{RunHistory: f.api.history}
	scoped := scopedHistory{inner: history, scope: &repoScope{repos: map[int64]model.Repository{f.repo.ID: f.repo}}}
	row, err := scoped.StepResult(t.Context(), run.ID, "test.unit")
	if err != nil || row.NodeID != "test.unit" || row.RunID != run.ID || history.queries != 1 {
		t.Fatal(row, err, history.queries)
	}
	scoped.scope = &repoScope{repos: map[int64]model.Repository{}}
	if _, err := scoped.StepResult(t.Context(), run.ID, "test.unit"); !errors.Is(err, store.ErrNotFound) || history.queries != 1 {
		t.Fatal("hidden run reached node lookup", err, history.queries)
	}
}
