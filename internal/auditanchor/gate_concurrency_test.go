package auditanchor

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/store"
	"github.com/oberthci/oberth/internal/testtimeout"
)

// Inject real commits after the snapshot and external checks, before the Store
// takes its fence. Manager serializes access to calls and afterVerification.
type racingGateStore struct {
	AnchorStore
	calls             int
	afterVerification func(context.Context) error
}

func (s *racingGateStore) callback(ctx context.Context, verify func(model.AuditHead, model.AuditAnchor) error) func(model.AuditHead, model.AuditAnchor) error {
	s.calls++
	return func(head model.AuditHead, anchor model.AuditAnchor) error {
		if err := verify(head, anchor); err != nil {
			return err
		}
		if s.afterVerification != nil {
			return s.afterVerification(ctx)
		}
		return nil
	}
}

func (s *racingGateStore) VerifyLocalAuditMutationState(ctx context.Context, prefix model.AuditHead, verify func(model.AuditHead, model.AuditAnchor) error) (model.AuditHead, model.AuditAnchor, error) {
	return s.AnchorStore.VerifyLocalAuditMutationState(ctx, prefix, s.callback(ctx, verify))
}

func (s *racingGateStore) VerifyAuditMutationState(ctx context.Context, intents []model.AuditWitnessIntent, witnesses []model.AuditWitness, verify func(model.AuditHead, model.AuditAnchor) error) (model.AuditHead, model.AuditAnchor, error) {
	return s.AnchorStore.VerifyAuditMutationState(ctx, intents, witnesses, s.callback(ctx, verify))
}

func (s *racingGateStore) VerifyAuditMutationStateUnanchored(ctx context.Context, intents []model.AuditWitnessIntent, witnesses []model.AuditWitness, verify func(model.AuditHead, model.AuditAnchor) error) (model.AuditHead, model.AuditAnchor, error) {
	return s.AnchorStore.VerifyAuditMutationStateUnanchored(ctx, intents, witnesses, s.callback(ctx, verify))
}

func concurrentGateManager(t *testing.T, mode string) (*Manager, *racingGateStore, *store.Store, string) {
	t.Helper()
	now := time.Now()
	path := filepath.Join(t.TempDir(), "audit.sqlite")
	database, err := store.OpenAdminClient(t.Context(), path, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	racing := &racingGateStore{AnchorStore: database}
	config := ManagerConfig{Store: racing, Interval: time.Minute, MaxAge: time.Hour, GateTimeout: testtimeout.Start(defaultGateTimeout)}
	if strings.Contains(mode, "timestamp") {
		config.Authority = &fakeAuthority{now: &now}
	}
	if strings.Contains(mode, "witness") {
		config.Witness = &fakeWitness{now: &now}
		config.Continuity = newTestContinuity(t)
	}
	manager, err := NewManager(config)
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Initialize(t.Context()); err != nil {
		t.Fatal(err)
	}
	return manager, racing, database, path
}

func appendGateAction(ctx context.Context, database *store.Store) error {
	_, err := database.AppendAuditAction(ctx, model.AuditActionSpec{Actor: "test", Action: "concurrent.mutation", ResourceType: "test"})
	return err
}

func TestMutationGateConcurrentMutationsSurviveRepeatedHeadAdvances(t *testing.T) {
	for _, mode := range []string{"local", "timestamp", "witness", "timestamp+witness"} {
		t.Run(mode, func(t *testing.T) {
			manager, racing, database, _ := concurrentGateManager(t, mode)
			const mutations, collisions = 12, 5
			racing.afterVerification = func(ctx context.Context) error {
				if racing.calls <= collisions {
					return appendGateAction(ctx, database)
				}
				return nil
			}
			ctx, cancel := context.WithTimeout(t.Context(), testtimeout.Start(20*time.Second))
			defer cancel()
			start := make(chan struct{})
			done := make(chan error, mutations)
			for range mutations {
				go func() {
					<-start
					if err := manager.AllowMutation(ctx); err != nil {
						done <- err
						return
					}
					done <- appendGateAction(ctx, database)
				}()
			}
			close(start)
			for range mutations {
				if err := <-done; err != nil {
					t.Errorf("concurrent mutation failed: %v", err)
				}
			}
			head, err := database.VerifyAuditState(t.Context())
			if err != nil || head.ID != mutations+collisions || racing.calls < mutations+collisions {
				t.Fatalf("head=%d attempts=%d err=%v", head.ID, racing.calls, err)
			}
			if witness, ok := manager.witness.(*fakeWitness); ok && witness.historyCalls < racing.calls {
				t.Fatalf("fresh attempts reused stale external history: history=%d attempts=%d", witness.historyCalls, racing.calls)
			}
		})
	}
}

func TestMutationGateContentionHonorsDeadlineAndCancellation(t *testing.T) {
	for _, cancelEarly := range []bool{false, true} {
		t.Run(map[bool]string{false: "deadline", true: "cancellation"}[cancelEarly], func(t *testing.T) {
			manager, racing, database, _ := concurrentGateManager(t, "local")
			manager.gateTimeout = testtimeout.Scale(150 * time.Millisecond)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			racing.afterVerification = func(ctx context.Context) error {
				err := appendGateAction(ctx, database)
				if cancelEarly {
					cancel()
					return store.ErrAuditVerificationBusy
				}
				return err
			}
			started := time.Now()
			err := manager.AllowMutation(ctx)
			want := context.DeadlineExceeded
			if cancelEarly {
				want = context.Canceled
			}
			if !errors.Is(err, want) || !errors.Is(err, store.ErrAuditVerificationBusy) || time.Since(started) > testtimeout.Scale(3*time.Second) {
				t.Fatalf("contention not bounded/classified: attempts=%d elapsed=%s err=%v", racing.calls, time.Since(started), err)
			}
			manager.gateTimeout = testtimeout.Start(defaultGateTimeout)
			racing.afterVerification = nil
			if err := manager.AllowMutation(t.Context()); err != nil {
				t.Fatalf("expired gate leaked lock or permission: %v", err)
			}
		})
	}
}

func TestMutationGateTamperingAfterContentionFailsClosed(t *testing.T) {
	for _, mode := range []string{"local", "timestamp+witness"} {
		t.Run(mode, func(t *testing.T) {
			manager, racing, database, path := concurrentGateManager(t, mode)
			raw, err := sql.Open("sqlite", path)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = raw.Close() }()
			racing.afterVerification = func(ctx context.Context) error {
				if racing.calls == 1 {
					return appendGateAction(ctx, database)
				}
				// The fresh attempt must detect prefix tampering after a retry,
				// even with an unchanged head digest and a previously valid proof.
				_, err := raw.ExecContext(ctx, `DROP TRIGGER audit_actions_no_update; UPDATE audit_actions SET actor='forged' WHERE id=1`)
				return err
			}
			err = manager.AllowMutation(t.Context())
			if err == nil || !strings.Contains(err.Error(), "audit action 1 hash is invalid") || errors.Is(err, store.ErrAuditVerificationBusy) || racing.calls != 2 {
				t.Fatalf("tampering retried/accepted: attempts=%d err=%v", racing.calls, err)
			}
		})
	}
}

func TestMutationGateDoesNotRetryChangedProof(t *testing.T) {
	manager, racing, _, _ := concurrentGateManager(t, "local")
	racing.afterVerification = func(context.Context) error { return store.ErrAuditStateChanged }
	if err := manager.AllowMutation(t.Context()); !errors.Is(err, store.ErrAuditStateChanged) || errors.Is(err, store.ErrAuditVerificationBusy) || racing.calls != 1 {
		t.Fatalf("non-contention change retried: attempts=%d err=%v", racing.calls, err)
	}
}
