package service

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/store"
)

type retryEnqueueObserver struct{ accepted string }

func (observer *retryEnqueueObserver) AcceptEnqueue(_ context.Context, value model.EnqueueRunResult) error {
	observer.accepted = value.ID
	return nil
}
func (*retryEnqueueObserver) NotifyQueue() {}
func (*retryEnqueueObserver) DeliverPublication(context.Context, string) (model.PublicationFinalization, error) {
	return model.PublicationFinalization{}, nil
}
func (*retryEnqueueObserver) NotifyPublicationRecovery(string) {}

func TestUnifiedRerunChecksMutationGateAndRemovedTool(t *testing.T) {
	h := newRerunHarness(t, infraFailure)
	source, err := h.scheduler.EnqueueCI(t.Context(), CIRequest{EventID: "manual-replay", Repository: h.fixture.repo, Branch: rerunBranch, SHA: rerunSHA, Actor: "pusher"})
	if err != nil {
		t.Fatal(err)
	}
	h.process(t)
	args, _ := json.Marshal(map[string]string{"id": source.ID[:8]})
	h.service.mutationGate = func(context.Context) error { return ErrForbidden }
	if _, err := h.service.CallTool(t.Context(), rerunUplink, "run_rerun", args); !errors.Is(err, ErrForbidden) {
		t.Fatalf("mutation gate = %v", err)
	}
	h.service.mutationGate = nil
	if _, err := h.service.CallTool(t.Context(), rerunUplink, "run_retry", args); err == nil {
		t.Fatal("removed tool is still callable")
	}
	result := h.rerun(t, rerunUplink, map[string]any{"id": source.ID[:8]})
	run := h.run(t, result.RunID)
	if run.ID == source.ID || run.Actor != rerunUplink.Identity || run.SHA != source.SHA {
		t.Fatalf("rerun changed source identity: %+v", run)
	}
}

func TestUnifiedRerunRespectsUplinkScope(t *testing.T) {
	fixture := newScopeFixture(t)
	observer := &retryEnqueueObserver{}
	fixture.service.runReruns = fixture.store
	fixture.service.enqueues = observer
	var hidden, visible model.Run
	for _, runs := range fixture.runs {
		for _, run := range runs {
			if run.RepoID == fixture.hidden.ID {
				hidden = run
			}
			if run.RepoID == fixture.visible.ID {
				visible = run
			}
		}
	}
	for _, run := range []model.Run{hidden, visible} {
		if _, err := fixture.store.FinishRun(t.Context(), run.ID, model.RunResult{Status: model.RunFailed, Phase: "infrastructure", Error: "checkout unavailable"}); err != nil {
			t.Fatal(err)
		}
	}
	for _, id := range []string{hidden.ID, strings.Repeat("f", 32)} {
		if _, err := fixture.call(scopedActor, "run_rerun", map[string]string{"id": id}); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("hidden or missing run: %v", err)
		}
	}
	if observer.accepted != "" {
		t.Fatalf("hidden run was queued: %s", observer.accepted)
	}
	// Scope resolution must happen before a branch-head lookup or any enqueue.
	fixture.service.refs = nil
	if _, err := fixture.call(scopedAdmin, "run_rerun", map[string]string{"id": visible.ID}); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("visible run did not reach the branch-head guard: %v", err)
	}
}
