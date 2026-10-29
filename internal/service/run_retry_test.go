package service

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/api"
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

func TestRunRetryToolQueuesRunAndChecksMutationGate(t *testing.T) {
	fixture := newControlFixture(t)
	service := fixture.api(t)
	service.runRetries = fixture.store
	ctx := context.Background()
	source, err := fixture.store.EnqueueRun(ctx, model.RunSpec{RepoID: fixture.repo.ID,
		RefKind: model.RefBranch, Ref: "feature/retry", SHA: strings.Repeat("a", 40), Actor: "pusher", Trigger: "branch"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.store.FinishRun(ctx, source.ID, model.RunResult{Status: model.RunInterrupted}); err != nil {
		t.Fatal(err)
	}
	actor := api.Actor{Identity: "reviewer@host"}
	args, _ := json.Marshal(map[string]string{"id": source.ID[:8]})
	service.mutationGate = func(context.Context) error { return ErrForbidden }
	if _, err := service.CallTool(ctx, actor, "run_retry", args); !errors.Is(err, ErrForbidden) {
		t.Fatalf("mutation gate = %v", err)
	}
	service.mutationGate = nil
	value, err := service.CallTool(ctx, actor, "run_retry", args)
	if err != nil {
		t.Fatal(err)
	}
	run, ok := value.(model.Run)
	if !ok || run.ID == source.ID || run.Actor != actor.Identity || run.SHA != source.SHA || run.Status != model.RunQueued {
		t.Fatalf("retry result = %#v", value)
	}
}

func TestRunRetryToolRespectsUplinkScope(t *testing.T) {
	fixture := newScopeFixture(t)
	observer := &retryEnqueueObserver{}
	fixture.service.runRetries = fixture.store
	fixture.service.enqueues = observer
	ctx := context.Background()
	hidden := fixture.runs["codeberg"][0]
	visible := fixture.runs["github"][1]
	for _, run := range []model.Run{hidden, visible} {
		if _, err := fixture.store.FinishRun(ctx, run.ID, model.RunResult{Status: model.RunFailed,
			Phase: "infrastructure", Error: "checkout unavailable"}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := fixture.call(scopedActor, "run_retry", map[string]string{"id": hidden.ID}); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("hidden run retry = %v", err)
	}
	if _, err := fixture.call(scopedActor, "run_retry", map[string]string{"id": strings.Repeat("f", 32)}); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("missing run retry = %v", err)
	}
	if observer.accepted != "" {
		t.Fatalf("hidden run entered queue: %s", observer.accepted)
	}
	value, err := fixture.call(scopedActor, "run_retry", map[string]string{"id": visible.ID})
	if err != nil {
		t.Fatal(err)
	}
	run, ok := value.(model.Run)
	if !ok || run.Actor != scopedActor.Identity || run.RepoID != fixture.visible.ID || observer.accepted != run.ID {
		t.Fatalf("visible retry = %#v, accepted = %q", value, observer.accepted)
	}
}
