package service

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/api"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/pkg/pipeline"
)

func TestNewHeadSupersedesQueuedPriorityAndInheritsOverride(t *testing.T) {
	f := newControlFixture(t)
	ctx := t.Context()
	old, err := f.scheduler.EnqueueCI(ctx, CIRequest{EventID: "guard-old", Repository: f.repo, Branch: "feature/priority", SHA: strings.Repeat("a", 40), Actor: "requester"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.store.BumpRunPriority(ctx, "admin", old.ID, model.PriorityHighest, "urgent"); err != nil {
		t.Fatal(err)
	}
	newer, err := f.scheduler.EnqueueCI(ctx, CIRequest{EventID: "guard-new", Repository: f.repo, Branch: old.Ref, SHA: strings.Repeat("b", 40), Actor: "other"})
	if err != nil {
		t.Fatal(err)
	}
	got, err := f.store.Run(ctx, old.ID)
	if err != nil || got.Status != model.RunInterrupted || got.SupersededBy != newer.ID || len(newer.Cancellations) != 0 || newer.SupersededCount != 1 || newer.Priority != model.PriorityHigh || newer.PriorityRequestedBy != "admin" || newer.PriorityOverrideReason != "urgent" {
		t.Fatalf("queued priority supersession or inheritance failed: %+v, %v", got, err)
	}
}

func TestPriorityGuardBackfillCannotDelayHead(t *testing.T) {
	g := newAdmissionGate(1)
	holder := g.reserve(1)
	release, err := holder.acquire(context.Background(), pipeline.M)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	head := priorityWaiter(g, 2, model.PriorityHighest, time.Now(), pipeline.L)
	small := priorityWaiter(g, 3, model.PriorityHighest, time.Now(), pipeline.M)
	if granted(g, small) {
		t.Fatal("a later run overtook the priority head")
	}
	release()
	if !granted(g, head) || granted(g, small) {
		t.Fatal("priority head did not start next")
	}
	head.release()
	small.release()
}

func TestPriorityGuardSubmissionLeavesExistingRunsAndStartsNext(t *testing.T) {
	f := newGroupFixture(t, 1)
	for _, ref := range []string{"busy", "normal-a", "normal-b", "urgent"} {
		f.engine.declare(ref, "", pipeline.L)
		f.engine.declareCPU(ref, 5900)
	}
	running := f.branch(t, f.terraform, "busy", "a")
	f.start(t)
	if got := f.awaitCreated(t, 1); !got[running.ID] {
		t.Fatal(got)
	}
	a := f.branch(t, f.terraform, "normal-a", "b")
	b := f.branch(t, f.exampleService, "normal-b", "c")
	before := map[string]model.Run{}
	for _, r := range []model.Run{running, a, b} {
		got, err := f.store.Run(t.Context(), r.ID)
		if err != nil {
			t.Fatal(err)
		}
		before[r.ID] = got
	}
	admin := true
	urgent, err := f.scheduler.EnqueueCI(t.Context(), CIRequest{PriorityRequest: model.PriorityRequest{Priority: model.PriorityHighest, PriorityReason: "release blocker", UnscopedAdmin: &admin}, EventID: "priority-guard", Repository: f.exampleService, Branch: "urgent", SHA: strings.Repeat("d", 40), Actor: "operator"})
	if err != nil {
		t.Fatal(err)
	}
	for id, was := range before {
		got, err := f.store.Run(t.Context(), id)
		if err != nil || got.Status != was.Status || got.SupersededBy != "" {
			t.Fatalf("submission changed existing run %s: %+v %v", id, got, err)
		}
	}
	if len(urgent.Cancellations) != 0 {
		t.Fatal("priority submission created cancellations")
	}
	f.assertNothingCreated(t)
	f.engine.finish(t, running.ID)
	if got := f.awaitCreated(t, 1); !got[urgent.ID] {
		t.Fatalf("priority was not next: %v", got)
	}
	f.engine.finish(t, urgent.ID)
	for _, r := range []model.Run{a, b} {
		if got := f.awaitCreated(t, 1); !got[r.ID] {
			t.Fatalf("normal FIFO lost: %v", got)
		}
		f.engine.finish(t, r.ID)
	}
	f.awaitTerminal(t, b.ID)
	f.engine.assertNoViolations(t)
}

func TestPriorityGuardAuthorizationFallbackAndGrantLifecycle(t *testing.T) {
	f := newControlFixture(t)
	control, err := NewAPI(APIConfig{Runs: f.store, QueuePriorities: f.store, Enqueues: f.scheduler})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = control.Shutdown(t.Context()) }()
	admin := api.Actor{Identity: "admin", Admin: true}
	for _, tool := range []string{"priority_grant", "priority_grant_list", "priority_grant_revoke"} {
		_, err := control.CallTool(t.Context(), api.Actor{Identity: "scoped-admin", Admin: true, Scope: []string{"codeberg"}}, tool, json.RawMessage(`{}`))
		if !errors.Is(err, ErrForbidden) {
			t.Fatalf("scoped admin %s: %v", tool, err)
		}
	}

	enqueue := func(ref string, p model.RunPriority) model.Run {
		t.Helper()
		r, err := f.scheduler.EnqueueCI(t.Context(), CIRequest{PriorityRequest: model.PriorityRequest{Priority: p, PriorityReason: "urgent fix"}, EventID: ref, Repository: f.repo, Branch: ref, SHA: strings.Repeat("a", 40), Actor: "worker"})
		if err != nil {
			t.Fatal(err)
		}
		return r.Run
	}
	refused := enqueue("no-grant", model.PriorityHighest)
	if refused.Priority != model.PriorityNormal || !strings.Contains(refused.PriorityRefusal, "no admin grant") || refused.PriorityRequestedBy != "worker" {
		t.Fatalf("unauthorized priority accepted: %+v", refused)
	}
	raw := json.RawMessage(`{"identity":"worker","max_class":"promotion","expires":"` + time.Now().Add(time.Hour).UTC().Format(time.RFC3339) + `"}`)
	if _, err = control.CallTool(t.Context(), api.Actor{Identity: "worker"}, "priority_grant", raw); !errors.Is(err, ErrForbidden) {
		t.Fatalf("non-admin grant: %v", err)
	}
	if _, err = control.CallTool(t.Context(), admin, "priority_grant", raw); err != nil {
		t.Fatal(err)
	}
	high := enqueue("granted", model.PriorityPromotion)
	if high.Priority != model.PriorityPromotion || high.PriorityReason != "submission priority: urgent fix" {
		t.Fatalf("grant ignored: %+v", high)
	}
	above := enqueue("above-grant", model.PriorityHighest)
	if above.Priority != model.PriorityNormal || !strings.Contains(above.PriorityRefusal, "exceeds") {
		t.Fatalf("grant ceiling ignored: %+v", above)
	}
	if _, err = control.CallTool(t.Context(), admin, "priority_grant_list", json.RawMessage(`{}`)); err != nil {
		t.Fatal(err)
	}
	if _, err = control.CallTool(t.Context(), admin, "priority_grant_revoke", json.RawMessage(`{"identity":"worker"}`)); err != nil {
		t.Fatal(err)
	}
	after := enqueue("revoked", model.PriorityPromotion)
	if after.Priority != model.PriorityNormal {
		t.Fatal("revoked grant retained")
	}
	retained, err := f.store.Run(t.Context(), high.ID)
	if err != nil || retained.Priority != model.PriorityPromotion {
		t.Fatalf("revocation changed accepted build: %+v %v", retained, err)
	}
	audits, _, err := f.store.ListAuditActions(t.Context(), model.AuditListFilter{ActionPrefix: "run.priority.request"})
	if err != nil || len(audits) != 4 {
		t.Fatalf("priority requests not audited: %d %v", len(audits), err)
	}
}

func TestPriorityCancellationRequiresRequesterOrUnscopedAdmin(t *testing.T) {
	f := newControlFixture(t)
	control, err := NewAPI(APIConfig{Runs: f.store, History: f.store, RunCancels: f.store, Enqueues: f.scheduler})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = control.Shutdown(t.Context()) }()
	admin := true
	run, err := f.scheduler.EnqueueCI(t.Context(), CIRequest{PriorityRequest: model.PriorityRequest{Priority: model.PriorityHigh, UnscopedAdmin: &admin}, EventID: "cancel-priority", Repository: f.repo, Branch: "urgent", SHA: strings.Repeat("a", 40), Actor: "requester"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = control.CancelRun(t.Context(), api.Actor{Identity: "other"}, run.ID, "stop", false); !errors.Is(err, ErrForbidden) {
		t.Fatalf("other requester cancelled priority: %v", err)
	}
	if _, err = control.CancelRun(t.Context(), api.Actor{Identity: "requester"}, run.ID, "stop", false); err != nil {
		t.Fatal(err)
	}
}

func TestPrioritySubmissionReasonCannotImpersonateAutomaticTagClass(t *testing.T) {
	f := newControlFixture(t)
	admin := true
	enqueued, err := f.scheduler.EnqueueCI(t.Context(), CIRequest{PriorityRequest: model.PriorityRequest{Priority: model.PriorityHighest, PriorityReason: model.TagReleasePriorityReason, UnscopedAdmin: &admin}, EventID: "priority-reason-marker", Repository: f.repo, Branch: "urgent", SHA: strings.Repeat("a", 40), Actor: "operator"})
	if err != nil {
		t.Fatal(err)
	}
	r := f.scheduler.admission.reserveRun(enqueued.Run)
	f.scheduler.admission.mu.Lock()
	r.prepared = true
	p := r.priorityRun().Priority
	f.scheduler.admission.mu.Unlock()
	if p != model.PriorityHighest {
		t.Fatalf("submission reason downgraded explicit priority to %s", p)
	}
	r.cancel()
}
