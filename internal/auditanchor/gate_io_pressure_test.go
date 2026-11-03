package auditanchor

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/store"
	"github.com/oberthci/oberth/internal/testtimeout"
)

type slowGateStore struct {
	AnchorStore
	calls  int
	before func(context.Context, int) error
}

func (s *slowGateStore) wait(ctx context.Context) error {
	s.calls++
	return s.before(ctx, s.calls)
}

func (s *slowGateStore) VerifyLocalAuditMutationState(ctx context.Context, prefix model.AuditHead, verify func(model.AuditHead, model.AuditAnchor) error) (model.AuditHead, model.AuditAnchor, error) {
	if err := s.wait(ctx); err != nil {
		return model.AuditHead{}, model.AuditAnchor{}, err
	}
	return s.AnchorStore.VerifyLocalAuditMutationState(ctx, prefix, verify)
}

func (s *slowGateStore) VerifyAuditMutationState(ctx context.Context, intents []model.AuditWitnessIntent, history []model.AuditWitness, verify func(model.AuditHead, model.AuditAnchor) error) (model.AuditHead, model.AuditAnchor, error) {
	if err := s.wait(ctx); err != nil {
		return model.AuditHead{}, model.AuditAnchor{}, err
	}
	return s.AnchorStore.VerifyAuditMutationState(ctx, intents, history, verify)
}

func (s *slowGateStore) VerifyAuditMutationStateUnanchored(ctx context.Context, intents []model.AuditWitnessIntent, history []model.AuditWitness, verify func(model.AuditHead, model.AuditAnchor) error) (model.AuditHead, model.AuditAnchor, error) {
	if err := s.wait(ctx); err != nil {
		return model.AuditHead{}, model.AuditAnchor{}, err
	}
	return s.AnchorStore.VerifyAuditMutationStateUnanchored(ctx, intents, history, verify)
}

func injectedVerificationTimeout(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, testtimeout.Scale(20*time.Millisecond))
	defer cancel()
	<-ctx.Done()
	return ctx.Err()
}

func TestMutationGateRetriesSlowVerification(t *testing.T) {
	for _, mode := range []string{"local", "timestamp", "witness", "timestamp+witness"} {
		t.Run(mode, func(t *testing.T) {
			manager, _, _, _ := concurrentGateManager(t, mode)
			slow := &slowGateStore{AnchorStore: manager.store, before: func(ctx context.Context, attempt int) error {
				if attempt == 1 {
					return injectedVerificationTimeout(ctx)
				}
				return nil
			}}
			manager.store = slow
			started := time.Now()
			if err := manager.AllowMutation(t.Context()); err != nil || slow.calls != 2 {
				t.Fatalf("slow verification rejected instead of retried: calls=%d err=%v", slow.calls, err)
			}
			t.Logf("injected verifier timeout=%s; successful gate elapsed=%s; attempts=%d", testtimeout.Scale(20*time.Millisecond), time.Since(started), slow.calls)
		})
	}
}

func TestMutationGateUsesConfiguredAttemptDeadline(t *testing.T) {
	manager, _, _, _ := concurrentGateManager(t, "local")
	manager.gateVerifyTimeout = testtimeout.Scale(50 * time.Millisecond)
	slow := &slowGateStore{AnchorStore: manager.store, before: func(ctx context.Context, attempt int) error {
		if attempt != 1 {
			return nil
		}
		started := time.Now()
		<-ctx.Done()
		if elapsed := time.Since(started); elapsed > testtimeout.Scale(3*time.Second) {
			t.Errorf("configured attempt deadline was ignored: %s", elapsed)
		}
		manager.gateVerifyTimeout = testtimeout.Start(2 * time.Second)
		return ctx.Err()
	}}
	manager.store = slow
	if err := manager.AllowMutation(t.Context()); err != nil || slow.calls != 2 {
		t.Fatalf("attempt did not receive a fresh budget after timeout: calls=%d err=%v", slow.calls, err)
	}
}

func TestMutationGateSlowVerificationHonorsCallerCancellation(t *testing.T) {
	manager, _, _, _ := concurrentGateManager(t, "local")
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	slow := &slowGateStore{AnchorStore: manager.store, before: func(ctx context.Context, _ int) error { cancel(); <-ctx.Done(); return ctx.Err() }}
	manager.store = slow
	if err := manager.AllowMutation(ctx); !errors.Is(err, context.Canceled) || errors.Is(err, ErrVerificationUnavailable) || slow.calls != 1 {
		t.Fatalf("caller cancellation retried or misclassified: calls=%d err=%v", slow.calls, err)
	}
}

func TestMutationGateSlowVerificationRetriesAreBounded(t *testing.T) {
	manager, _, _, _ := concurrentGateManager(t, "local")
	manager.gateTimeout = testtimeout.Start(2 * time.Second)
	slow := &slowGateStore{AnchorStore: manager.store, before: func(ctx context.Context, _ int) error { return injectedVerificationTimeout(ctx) }}
	manager.store = slow
	err := manager.AllowMutation(t.Context())
	if !errors.Is(err, context.DeadlineExceeded) || !errors.Is(err, ErrVerificationUnavailable) || slow.calls != 3 || !strings.Contains(err.Error(), "retry") {
		t.Fatalf("timeout must give retry guidance after three bounded attempts: calls=%d err=%v", slow.calls, err)
	}
	// A timed-out gate neither grants permission nor poisons readiness or leaks its lock.
	slow.before = func(context.Context, int) error { return nil }
	if err := manager.AllowMutation(t.Context()); err != nil {
		t.Fatal(err)
	}
}

func TestMutationGateDeadlineRetainsEarlierContention(t *testing.T) {
	manager, _, _, _ := concurrentGateManager(t, "local")
	manager.gateTimeout = testtimeout.Scale(200 * time.Millisecond)
	slow := &slowGateStore{AnchorStore: manager.store, before: func(ctx context.Context, attempt int) error {
		if attempt == 1 {
			return store.ErrAuditVerificationBusy
		}
		<-ctx.Done()
		return ctx.Err()
	}}
	manager.store = slow
	err := manager.AllowMutation(t.Context())
	if !errors.Is(err, context.DeadlineExceeded) || !errors.Is(err, store.ErrAuditVerificationBusy) || slow.calls != 2 {
		t.Fatalf("deadline lost earlier contention: attempts=%d err=%v", slow.calls, err)
	}
}

func TestMutationGateDetectsCorruptionAfterSlowVerification(t *testing.T) {
	for _, mode := range []string{"local", "timestamp+witness"} {
		t.Run(mode, func(t *testing.T) {
			manager, _, database, path := concurrentGateManager(t, mode)
			if err := appendGateAction(t.Context(), database); err != nil {
				t.Fatal(err)
			}
			raw, err := sql.Open("sqlite", path)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = raw.Close() }()
			slow := &slowGateStore{AnchorStore: manager.store, before: func(ctx context.Context, attempt int) error {
				if attempt != 1 {
					return nil
				}
				if _, err := raw.ExecContext(ctx, `DROP TRIGGER audit_actions_no_update; UPDATE audit_actions SET actor='forged' WHERE id=1`); err != nil {
					t.Fatal(err)
				}
				return injectedVerificationTimeout(ctx)
			}}
			manager.store = slow
			err = manager.AllowMutation(t.Context())
			if err == nil || !strings.Contains(err.Error(), "hash is invalid") || slow.calls != 2 || errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("corruption was hidden or retried: calls=%d err=%v", slow.calls, err)
			}
		})
	}
}

func TestMutationGateDoesNotRetryCorruptionOrCancellation(t *testing.T) {
	for _, failure := range []error{store.ErrInvalidState, errors.Join(store.ErrInvalidState, context.DeadlineExceeded), errors.Join(store.ErrInvalidState, store.ErrAuditVerificationBusy), context.Canceled} {
		manager, _, _, _ := concurrentGateManager(t, "local")
		slow := &slowGateStore{AnchorStore: manager.store, before: func(context.Context, int) error { return failure }}
		manager.store = slow
		if err := manager.AllowMutation(t.Context()); !errors.Is(err, failure) || errors.Is(err, ErrVerificationUnavailable) || slow.calls != 1 {
			t.Fatalf("terminal failure must refuse immediately: calls=%d err=%v", slow.calls, err)
		}
	}
}
