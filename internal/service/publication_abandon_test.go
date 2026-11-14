package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/api"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/storetest"
)

func pendingAbandonFixture(t *testing.T) (*controlFixture, model.Publication) {
	t.Helper()
	f := newControlFixture(t)
	run, err := f.store.EnqueueRun(t.Context(), model.RunSpec{RepoID: f.repo.ID, RefKind: model.RefBranch, Ref: "feature/abandon", SHA: retrySourceSHA, Actor: "agent@host", Trigger: "branch"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = storetest.ClaimNextRun(f.store, t.Context()); err != nil {
		t.Fatal(err)
	}
	pub, err := f.store.BeginPublication(t.Context(), model.PublicationSpec{RepoID: f.repo.ID, RunID: run.ID, RefKind: run.RefKind, Ref: run.Ref, ResultSHA: run.SHA, Actor: run.Actor})
	if err != nil {
		t.Fatal(err)
	}
	pub, err = f.store.UpdatePublicationAttempt(t.Context(), pub.ID, 0, 1, time.Now().Add(time.Hour), "upstream publication retry 2/5 queued")
	if err != nil {
		t.Fatal(err)
	}
	return f, pub
}

func TestPublicationAbandonIsAdminOnlyAuditedAndNeverPushes(t *testing.T) {
	f, pub := pendingAbandonFixture(t)
	control := f.api(t)
	args, _ := json.Marshal(map[string]any{"id": pub.ID, "reason": "operator stopped delivery"})
	if _, err := control.CallTool(t.Context(), api.Actor{Identity: "agent@host"}, "publication_abandon", args); !errors.Is(err, ErrForbidden) {
		t.Fatalf("non-admin abandon: %v", err)
	}
	wake := f.scheduler.signals.Run(pub.RunID)
	value, err := control.CallTool(t.Context(), api.Actor{Identity: "admin@host", Admin: true}, "publication_abandon", args)
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(value)
	if !bytes.Contains(encoded, []byte(`"action":"publication_abandon"`)) {
		t.Fatalf("response: %s", encoded)
	}
	state, err := f.store.PublicationState(t.Context(), pub.ID)
	if err != nil || state.Publication.Status != "abandoned" || state.Run.Status != model.RunFailed || state.Run.Phase != "abandoned" || state.Run.FinishedAt == nil || !strings.Contains(state.Run.Reason, "admin@host: operator stopped delivery") {
		t.Fatalf("abandoned state: %+v, %v", state, err)
	}
	audit, err := f.store.LatestAuditActionByType(t.Context(), "publication.abandon", "publication")
	if err != nil || audit.Actor != "admin@host" || audit.ResourceID != pub.ID || !strings.Contains(audit.Details, "operator stopped delivery") {
		t.Fatalf("abandon audit: %+v %v", audit, err)
	}
	select {
	case <-wake:
	default:
		t.Fatal("run waiters not notified")
	}
	if _, err := control.CallTool(t.Context(), api.Actor{Identity: "admin@host", Admin: true}, "publication_abandon", args); err == nil {
		t.Fatal("terminal abandon accepted")
	}
	if _, err := f.store.RetryFailedPublication(t.Context(), pub.RunID, "", "admin@host"); err == nil {
		t.Fatal("abandoned publication reactivated")
	}
	// A queued retry or startup recovery must observe the terminal state.
	f.scheduler.NotifyPublicationRecovery(pub.ID)
	if err := f.scheduler.recoverNotifiedPublicationsWithGate(t.Context(), f.scheduler.requireMutation); err != nil {
		t.Fatal(err)
	}
	if err := f.scheduler.recoverPendingPublications(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(f.git.syncedBranches) != 0 || len(f.git.syncedTags) != 0 || len(f.git.promotions) != 0 {
		t.Fatal("abandon or recovery pushed")
	}
}

func TestPublicationAbandonRefusesScopedActorsBadReasonsAndClosedGate(t *testing.T) {
	f, pub := pendingAbandonFixture(t)
	control := f.api(t)
	admin := api.Actor{Identity: "admin@host", Admin: true}
	scoped := admin
	scoped.Scope = []string{"codeberg"}
	args, _ := json.Marshal(map[string]any{"id": pub.ID, "reason": "stop"})
	if _, err := control.CallTool(t.Context(), scoped, "publication_abandon", args); !errors.Is(err, ErrForbidden) {
		t.Fatalf("scoped admin: %v", err)
	}
	for _, reason := range []string{"", "two\nlines", strings.Repeat("x", 513)} {
		raw, _ := json.Marshal(map[string]any{"id": pub.ID, "reason": reason})
		if _, err := control.CallTool(t.Context(), admin, "publication_abandon", raw); !errors.Is(err, ErrInvalidInput) {
			t.Fatalf("bad reason %q: %v", reason, err)
		}
	}
	blocked := errors.New("audit gate closed")
	control.mutationGate = func(context.Context) error { return blocked }
	if _, err := control.CallTool(t.Context(), admin, "publication_abandon", args); !errors.Is(err, blocked) {
		t.Fatalf("closed API gate: %v", err)
	}
	// The scheduler repeats the gate after obtaining the delivery permit.
	f.scheduler.mutationGate = func(context.Context) error { return blocked }
	if _, err := f.scheduler.AbandonPublication(t.Context(), pub.ID, admin.Identity, "stop"); !errors.Is(err, blocked) {
		t.Fatalf("closed scheduler gate: %v", err)
	}
	state, err := f.store.PublicationState(t.Context(), pub.ID)
	if err != nil || state.Publication.Status != model.PublicationPending || state.Run.Status != model.RunRunning {
		t.Fatalf("refused abandon changed state: %+v %v", state, err)
	}
}

func TestPublicationAbandonWaitsForDeliveryPermit(t *testing.T) {
	f, pub := pendingAbandonFixture(t)
	<-f.scheduler.deliveryPermit
	defer func() { f.scheduler.deliveryPermit <- struct{}{} }()
	ctx, cancel := context.WithTimeout(t.Context(), testTimeout(100*time.Millisecond))
	defer cancel()
	if _, err := f.scheduler.AbandonPublication(ctx, pub.ID, "admin@host", "stop"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("abandon bypassed in-flight delivery: %v", err)
	}
	state, err := f.store.PublicationState(t.Context(), pub.ID)
	if err != nil || state.Publication.Status != model.PublicationPending {
		t.Fatalf("waiting abandon changed state: %+v %v", state, err)
	}
}

func TestPublicationAbandonFailsPromotionWithoutPush(t *testing.T) {
	f := newControlFixture(t)
	promotion, pub := beginPendingPromotionPublication(t, f, "main", retryBaseSHA, retrySourceSHA)
	final, err := f.scheduler.AbandonPublication(t.Context(), pub.ID, "admin@host", "stop promotion delivery")
	if err != nil || final.Publication.Status != model.PublicationAbandoned || final.Promotion.ID != promotion.ID || final.Promotion.Status != model.PromotionFailed || !strings.Contains(final.Promotion.Error, "admin@host: stop promotion delivery") {
		t.Fatalf("abandoned promotion: %+v %v", final, err)
	}
	if pub.RunID != "" && (final.Run.Status != model.RunFailed || final.Run.Phase != "abandoned") {
		t.Fatalf("promotion run: %+v", final.Run)
	}
	if err := f.scheduler.recoverPendingPublications(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(f.git.promotions) != 0 {
		t.Fatal("abandon pushed promotion")
	}
}
