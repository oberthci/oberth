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
	"github.com/oberthci/oberth/pkg/argoworkflow"
	"github.com/oberthci/oberth/pkg/periapsis"
)

func TestPriorityGuardSupersession(t *testing.T) {
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
	if err != nil || got.Status != model.RunQueued || got.SupersededBy != "" || len(newer.Cancellations) != 0 {
		t.Fatalf("priority cancelled by newer push: %+v, %v", got, err)
	}
}

func TestPriorityGuardBackfillCannotDelayHead(t *testing.T) {
	g := newWeightedAdmission(4)
	g.maxHeadWait = 20 * time.Minute
	holder := g.reserve(1)
	release, err := holder.acquire(context.Background(), periapsis.M)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	head := priorityWaiter(g, 2, model.PriorityHighest, time.Now(), periapsis.L)
	small := priorityWaiter(g, 3, model.PriorityHighest, time.Now(), periapsis.M)
	if granted(g, small) {
		t.Fatal("backfill leaves too little capacity for the priority head")
	}
	release()
	if !granted(g, head) || granted(g, small) {
		t.Fatal("priority head did not start next")
	}
	head.release()
	small.release()
}

func TestPriorityGuardSubmissionLeavesExistingRunsAndStartsNext(t *testing.T) {
	f := newGroupFixture(t, 2)
	for _, ref := range []string{"busy", "normal-a", "normal-b", "urgent"} {
		f.engine.declare(ref, "", periapsis.L)
	}
	running := f.branch(t, f.terraform, "busy", "a")
	f.start(t)
	if got := f.awaitCreated(t, 1); !got[running.ID] {
		t.Fatal(got)
	}
	a := f.branch(t, f.terraform, "normal-a", "b")
	b := f.branch(t, f.exampleService, "normal-b", "c")
	f.awaitAdmission(t, a, "normal waiting", func(o *model.AdmissionObservation) bool { return o != nil && o.State == "waiting" })
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
	raw := json.RawMessage(`{"identity":"worker","max_class":"high","expires":"` + time.Now().Add(time.Hour).UTC().Format(time.RFC3339) + `"}`)
	if _, err = control.CallTool(t.Context(), api.Actor{Identity: "worker"}, "priority_grant", raw); !errors.Is(err, ErrForbidden) {
		t.Fatalf("non-admin grant: %v", err)
	}
	if _, err = control.CallTool(t.Context(), admin, "priority_grant", raw); err != nil {
		t.Fatal(err)
	}
	high := enqueue("granted", model.PriorityHigh)
	if high.Priority != model.PriorityHigh || high.PriorityReason != "submission priority: urgent fix" {
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
	after := enqueue("revoked", model.PriorityHigh)
	if after.Priority != model.PriorityNormal {
		t.Fatal("revoked grant retained")
	}
	retained, err := f.store.Run(t.Context(), high.ID)
	if err != nil || retained.Priority != model.PriorityHigh {
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

func TestPriorityBackfillPreservesNumericHeadAndAggregateDemand(t *testing.T) {
	for _, tc := range []struct {
		name                                               string
		headCPU, headMemory, candidateCPU, candidateMemory int64
		want                                               bool
	}{
		{"CPU fits", 5000, 1 << 30, 1000, 1 << 30, true},
		{"CPU exceeds", 7000, 1 << 30, 1000, 1 << 30, false},
		{"memory exceeds", 1000, 13 << 30, 1000, 3 << 30, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clock := &fakeClock{now: time.Date(2026, 11, 3, 0, 0, 0, 0, time.UTC)}
			g := newWeightedAdmission(8)
			g.now, g.maxHeadWait = clock.Now, 20*time.Minute
			running := g.reserve(1)
			acquireRequestWithinBudget(t, running, admissionRequest{Size: periapsis.M, Estimate: 30 * time.Minute,
				Pods: &argoworkflow.PodDemand{CPUMilli: 4000, MemoryBytes: 4 << 30, Pods: 1}})
			t.Cleanup(running.release)
			head := pending(t, g, 2, admissionRequest{Size: periapsis.M,
				Pods: &argoworkflow.PodDemand{CPUMilli: tc.headCPU, MemoryBytes: tc.headMemory, Pods: 1}})
			t.Cleanup(head.cancel)
			if granted(g, head) {
				t.Fatal("fixture head must wait before backfill")
			}
			g.mu.Lock()
			head.priority = model.PriorityHighest
			g.mu.Unlock()
			clock.now = clock.now.Add(21 * time.Minute)
			candidate := pendingPriority(t, g, 3, model.PriorityHighest, admissionRequest{Size: periapsis.M, Estimate: time.Hour,
				Pods: &argoworkflow.PodDemand{CPUMilli: tc.candidateCPU, MemoryBytes: tc.candidateMemory, Pods: 1}})
			t.Cleanup(candidate.cancel)
			if got := granted(g, candidate); got != tc.want {
				t.Fatalf("head fits alongside = %v, want %v", got, tc.want)
			}
		})
	}
	g := newWeightedAdmission(4)
	g.maxHeadWait = 20 * time.Minute
	running := g.reserve(1)
	acquireRequestWithinBudget(t, running, admissionRequest{Size: periapsis.M, Estimate: time.Hour})
	t.Cleanup(running.release)
	head := priorityWaiter(g, 2, model.PriorityHighest, time.Now(), periapsis.L)
	t.Cleanup(head.cancel)
	first := priorityWaiter(g, 3, model.PriorityHighest, time.Now(), periapsis.S, 2*time.Hour)
	t.Cleanup(first.cancel)
	second := priorityWaiter(g, 4, model.PriorityHighest, time.Now(), periapsis.S, 2*time.Hour)
	t.Cleanup(second.cancel)
	if !granted(g, first) || granted(g, second) {
		t.Fatal("aggregate backfills failed to preserve the priority head")
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
	r.size = periapsis.XL
	p := r.priorityRun().Priority
	f.scheduler.admission.mu.Unlock()
	if p != model.PriorityHighest {
		t.Fatalf("submission reason downgraded explicit priority to %s", p)
	}
	r.cancel()
}
