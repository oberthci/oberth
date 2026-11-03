package service

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/api"
	"github.com/oberthci/oberth/internal/model"
)

// A claim may reveal external-state work after the initial read. Exercise the
// transaction's authorization with that newer snapshot.
type priorityReviewCancelStore struct {
	RunCancelStore
	current model.Run
}

func (s priorityReviewCancelStore) CancelRun(ctx context.Context, request model.RunCancelRequest) (model.RunCancelResult, error) {
	if _, err := request.Authorize(s.current); err != nil {
		return model.RunCancelResult{}, err
	}
	return s.RunCancelStore.CancelRun(ctx, request)
}
func TestPriorityReviewRequesterNeverAcquiresAdminCancellation(t *testing.T) {
	for _, kind := range []string{"tag", "release", "promotion", "concurrency-group"} {
		t.Run(kind, func(t *testing.T) {
			f := newControlFixture(t)
			spec := model.RunSpec{RepoID: f.repo.ID, RefKind: model.RefBranch, Ref: "urgent", SHA: strings.Repeat("a", 40), Actor: "requester", Trigger: "branch"}
			run, err := f.store.EnqueueRun(t.Context(), spec)
			if err != nil {
				t.Fatal(err)
			}
			current := run.Run
			current.PriorityProtected, current.PriorityRequestedBy = true, "requester"
			switch kind {
			case "tag":
				current.RefKind, current.Trigger = model.RefTag, "tag"
			case "release":
				current.Release = true
			case "promotion":
				current.Trigger = "promotion"
			case "concurrency-group":
				current.ConcurrencyGroup = "external-state"
			}
			control, err := NewAPI(APIConfig{Runs: f.store, History: f.store, RunCancels: priorityReviewCancelStore{RunCancelStore: f.store, current: current}})
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = control.Shutdown(t.Context()) }()
			if _, err = control.CancelRun(t.Context(), api.Actor{Identity: "requester"}, run.ID, "stop", true); !errors.Is(err, ErrForbidden) {
				t.Fatalf("requester force-cancelled own %s run: %v", kind, err)
			}
			got, err := f.store.Run(t.Context(), run.ID)
			if err != nil || got.Status != model.RunQueued {
				t.Fatalf("refused cancel mutated run: %+v %v", got, err)
			}
		})
	}
}
