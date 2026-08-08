package auditanchor

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/store"

	"k8s.io/client-go/kubernetes/fake"
)

type fakeAuthority struct {
	now          *time.Time
	timestampErr error
	verifyErr    error
	verifyHook   func()
	calls        int
}

type fakeWitness struct {
	now          *time.Time
	history      []model.AuditWitness
	historyErr   error
	historyHook  func()
	publishErr   error
	historyCalls int
}

type countingContinuity struct {
	Continuity
	reconcileCalls int
	reconcileErr   error
	reconcileHook  func()
}

func newTestContinuity(t *testing.T) *KubernetesContinuity {
	t.Helper()
	continuity, err := NewKubernetesContinuity(fake.NewSimpleClientset(), "oberth")
	if err != nil {
		t.Fatal(err)
	}
	return continuity
}

func (witness *fakeWitness) History(context.Context, []model.AuditWitness, model.AuditHead) ([]model.AuditWitness, error) {
	witness.historyCalls++
	if witness.historyErr != nil {
		return nil, witness.historyErr
	}
	if witness.historyHook != nil {
		hook := witness.historyHook
		witness.historyHook = nil
		hook()
	}
	return append([]model.AuditWitness(nil), witness.history...), nil
}

func (continuity *countingContinuity) Reconcile(ctx context.Context, history []model.AuditWitness) error {
	continuity.reconcileCalls++
	if continuity.reconcileHook != nil {
		hook := continuity.reconcileHook
		continuity.reconcileHook = nil
		hook()
	}
	if continuity.reconcileErr != nil {
		return continuity.reconcileErr
	}
	return continuity.Continuity.Reconcile(ctx, history)
}

func TestMutationGateRequiresFreshExternalWitnessRead(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 8, 6, 16, 45, 0, 0, time.UTC)
	database, err := store.OpenAdminClient(ctx, filepath.Join(t.TempDir(), "oberth.sqlite"), store.Options{Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = database.Close() }()
	external := &fakeWitness{now: &now}
	manager, err := NewManager(ManagerConfig{
		Store: database, Authority: &fakeAuthority{now: &now}, Witness: external, Continuity: newTestContinuity(t),
		Interval: 10 * time.Minute, MaxAge: 30 * time.Minute, Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	manager.cycle(ctx)
	if err := manager.Ready(ctx); err != nil {
		t.Fatal(err)
	}
	callsBefore := external.historyCalls
	external.historyErr = fmt.Errorf("%w: Rekor offline", ErrWitnessUnavailable)
	if err := manager.AllowMutation(ctx); err == nil || !strings.Contains(err.Error(), "mutation gate refresh") {
		t.Fatalf("mutation during witness outage error = %v", err)
	}
	if external.historyCalls != callsBefore+1 {
		t.Fatalf("mutation gate external reads = %d, want %d", external.historyCalls, callsBefore+1)
	}
}

func TestMutationGateRejectsUnresolvedPrePublicationIntent(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 8, 6, 16, 47, 0, 0, time.UTC)
	database, err := store.OpenAdminClient(ctx, filepath.Join(t.TempDir(), "oberth.sqlite"), store.Options{Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = database.Close() }()
	external := &fakeWitness{now: &now}
	continuity := newTestContinuity(t)
	manager, err := NewManager(ManagerConfig{
		Store: database, Authority: &fakeAuthority{now: &now}, Witness: external, Continuity: continuity,
		Interval: 10 * time.Minute, MaxAge: 30 * time.Minute, Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	manager.cycle(ctx)
	if err := manager.Ready(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := database.AppendAuditAction(ctx, model.AuditActionSpec{
		Actor: "agent@host", Action: "issue.create", ResourceType: "issue", ResourceID: "pending-witness",
	}); err != nil {
		t.Fatal(err)
	}
	external.publishErr = fmt.Errorf("%w: rejected before remote side effect", ErrWitnessUnavailable)
	manager.cycle(ctx)
	if err := manager.Ready(ctx); err != nil {
		t.Fatalf("recent verified checkpoint should retain bounded readiness: %v", err)
	}
	if err := manager.AllowMutation(ctx); err == nil || !strings.Contains(err.Error(), "unresolved immutable witness intent") {
		t.Fatalf("mutation with unresolved witness intent error = %v", err)
	}

	external.publishErr = nil
	manager.cycle(ctx)
	if err := manager.Ready(ctx); err != nil {
		t.Fatalf("exact pending intent recovery: %v", err)
	}
	intents, err := continuity.Intents(ctx)
	if err != nil {
		t.Fatal(err)
	}
	pinned, err := continuity.Pinned(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(intents) != 2 || len(pinned) != 2 {
		t.Fatalf("intent/completion counts after recovery = %d/%d, want 2/2", len(intents), len(pinned))
	}
	if err := manager.AllowMutation(ctx); err != nil {
		t.Fatalf("mutation remained blocked after exact intent completion: %v", err)
	}
}

func TestManagerCompletesPendingPrefixBeforeAdvancedHead(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 8, 6, 16, 48, 0, 0, time.UTC)
	database, err := store.OpenAdminClient(ctx, filepath.Join(t.TempDir(), "oberth.sqlite"), store.Options{Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = database.Close() }()
	external := &fakeWitness{now: &now}
	manager, err := NewManager(ManagerConfig{
		Store: database, Authority: &fakeAuthority{now: &now}, Witness: external, Continuity: newTestContinuity(t),
		Interval: 10 * time.Minute, MaxAge: 30 * time.Minute, Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	manager.cycle(ctx)
	for _, resourceID := range []string{"intended", "bypassed-gate"} {
		if _, err := database.AppendAuditAction(ctx, model.AuditActionSpec{
			Actor: "agent@host", Action: "issue.create", ResourceType: "issue", ResourceID: resourceID,
		}); err != nil {
			t.Fatal(err)
		}
		if resourceID == "intended" {
			external.publishErr = fmt.Errorf("%w: rejected before remote side effect", ErrWitnessUnavailable)
			manager.cycle(ctx)
			external.publishErr = nil
		}
	}
	manager.cycle(ctx)
	if err := manager.Ready(ctx); err != nil {
		t.Fatalf("complete pending witnessed prefix: %v", err)
	}
	if len(external.history) != 2 || external.history[1].AuditID != 1 {
		t.Fatalf("first recovery history = %#v, want pending audit head 1", external.history)
	}
	manager.cycle(ctx)
	if err := manager.Ready(ctx); err != nil {
		t.Fatalf("witness advanced in-flight suffix: %v", err)
	}
	if len(external.history) != 3 || external.history[2].AuditID != 2 {
		t.Fatalf("second recovery history = %#v, want current audit head 2", external.history)
	}
	if err := manager.AllowMutation(ctx); err != nil {
		t.Fatalf("mutation blocked after pending prefix and suffix were witnessed: %v", err)
	}
}

func TestMutationGateRejectsDeletedPersistedCheckpointAfterReadiness(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 8, 6, 16, 50, 0, 0, time.UTC)
	path := filepath.Join(t.TempDir(), "oberth.sqlite")
	database, err := store.OpenAdminClient(ctx, path, store.Options{Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = database.Close() }()
	manager, err := NewManager(ManagerConfig{
		Store: database, Authority: &fakeAuthority{now: &now}, Witness: &fakeWitness{now: &now}, Continuity: newTestContinuity(t),
		Interval: 10 * time.Minute, MaxAge: 30 * time.Minute, Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	manager.cycle(ctx)
	if err := manager.Ready(ctx); err != nil {
		t.Fatal(err)
	}
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.ExecContext(ctx, `DROP TRIGGER audit_anchors_no_delete`); err != nil {
		_ = raw.Close()
		t.Fatal(err)
	}
	if _, err := raw.ExecContext(ctx, `DELETE FROM audit_anchors`); err != nil {
		_ = raw.Close()
		t.Fatal(err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := manager.AllowMutation(ctx); err == nil || !strings.Contains(err.Error(), "no persisted signed checkpoint") {
		t.Fatalf("mutation after persisted checkpoint deletion error = %v", err)
	}
}

func TestMutationGateReverifiesPersistedCheckpointReceipt(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 8, 6, 16, 55, 0, 0, time.UTC)
	path := filepath.Join(t.TempDir(), "oberth.sqlite")
	database, err := store.OpenAdminClient(ctx, path, store.Options{Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = database.Close() }()
	manager, err := NewManager(ManagerConfig{
		Store: database, Authority: &fakeAuthority{now: &now}, Witness: &fakeWitness{now: &now}, Continuity: newTestContinuity(t),
		Interval: 10 * time.Minute, MaxAge: 30 * time.Minute, Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	manager.cycle(ctx)
	if err := manager.Ready(ctx); err != nil {
		t.Fatal(err)
	}
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.ExecContext(ctx, `DROP TRIGGER audit_anchors_no_update`); err != nil {
		_ = raw.Close()
		t.Fatal(err)
	}
	if _, err := raw.ExecContext(ctx, `UPDATE audit_anchors SET receipt = X'00'`); err != nil {
		_ = raw.Close()
		t.Fatal(err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := manager.AllowMutation(ctx); err == nil || !strings.Contains(err.Error(), "invalid receipt") {
		t.Fatalf("mutation after persisted receipt tamper error = %v", err)
	}
}

func TestMutationGateDoesNotReconcileAfterStateChangesDuringWitnessRecovery(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 8, 6, 16, 57, 0, 0, time.UTC)
	path := filepath.Join(t.TempDir(), "oberth.sqlite")
	database, err := store.OpenAdminClient(ctx, path, store.Options{Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = database.Close() }()
	if _, err := database.AppendAuditAction(ctx, model.AuditActionSpec{
		Actor: "agent@host", Action: "issue.create", ResourceType: "issue", ResourceID: "1",
	}); err != nil {
		t.Fatal(err)
	}
	external := &fakeWitness{now: &now}
	continuity := &countingContinuity{Continuity: newTestContinuity(t)}
	manager, err := NewManager(ManagerConfig{
		Store: database, Authority: &fakeAuthority{now: &now}, Witness: external, Continuity: continuity,
		Interval: 10 * time.Minute, MaxAge: 30 * time.Minute, Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	manager.cycle(ctx)
	if err := manager.Ready(ctx); err != nil {
		t.Fatal(err)
	}
	continuity.reconcileCalls = 0
	var hookErr error
	external.historyHook = func() {
		raw, err := sql.Open("sqlite", path)
		if err != nil {
			hookErr = err
			return
		}
		defer func() { _ = raw.Close() }()
		if _, err := raw.ExecContext(ctx, `DROP TRIGGER audit_actions_no_update`); err != nil {
			hookErr = err
			return
		}
		_, hookErr = raw.ExecContext(ctx, `UPDATE audit_actions SET details = '{"tampered":true}' WHERE id = 1`)
	}
	err = manager.AllowMutation(ctx)
	if hookErr != nil {
		t.Fatal(hookErr)
	}
	if err == nil || !strings.Contains(err.Error(), "audit action 1 hash is invalid") {
		t.Fatalf("mutation after mid-recovery state change error = %v", err)
	}
	if continuity.reconcileCalls != 0 {
		t.Fatalf("rollback-external reconcile calls after rejected state = %d, want 0", continuity.reconcileCalls)
	}
}

func TestMutationGateHoldsSQLiteWriterLeaseThroughExternalChecks(t *testing.T) {
	for _, stage := range []string{"authority verification", "continuity reconciliation"} {
		t.Run(stage, func(t *testing.T) {
			ctx := context.Background()
			now := time.Date(2026, 8, 6, 16, 58, 0, 0, time.UTC)
			path := filepath.Join(t.TempDir(), "oberth.sqlite")
			database, err := store.OpenAdminClient(ctx, path, store.Options{Now: func() time.Time { return now }})
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = database.Close() }()
			if _, err := database.AppendAuditAction(ctx, model.AuditActionSpec{
				Actor: "agent@host", Action: "issue.create", ResourceType: "issue", ResourceID: "lease",
			}); err != nil {
				t.Fatal(err)
			}
			authority := &fakeAuthority{now: &now}
			continuity := &countingContinuity{Continuity: newTestContinuity(t)}
			manager, err := NewManager(ManagerConfig{
				Store: database, Authority: authority, Witness: &fakeWitness{now: &now}, Continuity: continuity,
				Interval: 10 * time.Minute, MaxAge: 30 * time.Minute, Now: func() time.Time { return now },
			})
			if err != nil {
				t.Fatal(err)
			}
			manager.cycle(ctx)
			if err := manager.Ready(ctx); err != nil {
				t.Fatal(err)
			}

			raw, err := sql.Open("sqlite", path)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = raw.Close() }()
			if _, err := raw.ExecContext(ctx, `PRAGMA busy_timeout = 1`); err != nil {
				t.Fatal(err)
			}
			if _, err := raw.ExecContext(ctx, `DROP TRIGGER audit_actions_no_update`); err != nil {
				t.Fatal(err)
			}

			attempted := false
			var writeErr error
			attemptWrite := func() {
				attempted = true
				_, writeErr = raw.ExecContext(ctx, `UPDATE audit_actions SET details = '{"tampered":true}' WHERE id = 1`)
			}
			if stage == "authority verification" {
				authority.verifyHook = attemptWrite
			} else {
				continuity.reconcileHook = attemptWrite
			}
			if err := manager.AllowMutation(ctx); err != nil {
				t.Fatalf("valid mutation gate: %v", err)
			}
			if !attempted || writeErr == nil {
				t.Fatalf("concurrent write during %s: attempted=%t error=%v", stage, attempted, writeErr)
			}
			message := strings.ToLower(writeErr.Error())
			if !strings.Contains(message, "locked") && !strings.Contains(message, "busy") {
				t.Fatalf("concurrent write during %s failed for an unexpected reason: %v", stage, writeErr)
			}
			if _, err := database.VerifyAuditChain(ctx); err != nil {
				t.Fatalf("blocked concurrent write changed the audit chain: %v", err)
			}
		})
	}
}

func TestMutationGateFailsWhenContinuityReconciliationFails(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 8, 6, 16, 59, 0, 0, time.UTC)
	database, err := store.OpenAdminClient(ctx, filepath.Join(t.TempDir(), "oberth.sqlite"), store.Options{Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = database.Close() }()
	continuity := &countingContinuity{Continuity: newTestContinuity(t)}
	manager, err := NewManager(ManagerConfig{
		Store: database, Authority: &fakeAuthority{now: &now}, Witness: &fakeWitness{now: &now}, Continuity: continuity,
		Interval: 10 * time.Minute, MaxAge: 30 * time.Minute, Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	manager.cycle(ctx)
	if err := manager.Ready(ctx); err != nil {
		t.Fatal(err)
	}
	continuity.reconcileErr = errors.New("continuity unavailable")
	if err := manager.AllowMutation(ctx); err == nil || !strings.Contains(err.Error(), "continuity unavailable") {
		t.Fatalf("mutation after continuity reconciliation failure error = %v", err)
	}
}

func (witness *fakeWitness) Publish(_ context.Context, head model.AuditHead) (model.AuditWitness, error) {
	if witness.publishErr != nil {
		return model.AuditWitness{}, witness.publishErr
	}
	if len(witness.history) > 0 {
		current := witness.history[len(witness.history)-1]
		if current.AuditID == head.ID && bytes.Equal(current.AuditSHA256, head.SHA256) {
			return current, nil
		}
	}
	entry := model.AuditWitness{
		UUID: fmt.Sprintf("witness-%d", len(witness.history)+1), LogIndex: int64(len(witness.history)),
		IntegratedAt: *witness.now, AuditID: head.ID, AuditSHA256: append([]byte(nil), head.SHA256...),
	}
	if len(witness.history) > 0 {
		entry.PreviousUUID = witness.history[len(witness.history)-1].UUID
	}
	witness.history = append(witness.history, entry)
	return entry, nil
}

func (authority *fakeAuthority) Timestamp(_ context.Context, head model.AuditHead) (model.AuditAnchorSpec, error) {
	authority.calls++
	if authority.timestampErr != nil {
		return model.AuditAnchorSpec{}, authority.timestampErr
	}
	return model.AuditAnchorSpec{
		AuditID: head.ID, AuditSHA256: append([]byte(nil), head.SHA256...),
		TSAURL: "https://tsa.example.test", Receipt: []byte("valid receipt"), AnchoredAt: *authority.now,
	}, nil
}

func (authority *fakeAuthority) Verify(anchor model.AuditAnchor) error {
	if authority.verifyHook != nil {
		hook := authority.verifyHook
		authority.verifyHook = nil
		hook()
	}
	if authority.verifyErr != nil {
		return authority.verifyErr
	}
	if !bytes.Equal(anchor.Receipt, []byte("valid receipt")) {
		return errors.New("invalid receipt")
	}
	return nil
}

func TestManagerAnchorsGenesisAndEnforcesFreshness(t *testing.T) {
	now := time.Date(2026, 8, 6, 15, 0, 0, 0, time.UTC)
	database, err := store.OpenAdminClient(context.Background(), filepath.Join(t.TempDir(), "oberth.sqlite"), store.Options{Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = database.Close() }()
	authority := &fakeAuthority{now: &now}
	witness := &fakeWitness{now: &now}
	manager, err := NewManager(ManagerConfig{
		Store: database, Authority: authority, Witness: witness, Continuity: newTestContinuity(t), Interval: 10 * time.Minute,
		MaxAge: 30 * time.Minute, Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	manager.cycle(context.Background())
	if err := manager.Ready(context.Background()); err != nil {
		t.Fatal(err)
	}
	anchor, err := database.LatestAuditAnchor(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if anchor.AuditID != 0 || !bytes.Equal(anchor.AuditSHA256, make([]byte, 32)) {
		t.Fatalf("genesis anchor = %#v", anchor)
	}
	if err := manager.AllowMutation(context.Background()); err != nil {
		t.Fatalf("genesis mutation gate: %v", err)
	}
	now = now.Add(31 * time.Minute)
	if err := manager.Ready(context.Background()); err == nil {
		t.Fatal("stale checkpoint passed readiness")
	}
}

func TestManagerVerifiesExistingStartupReadOnlyBeforeInitialization(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 8, 6, 15, 30, 0, 0, time.UTC)
	path := filepath.Join(t.TempDir(), "oberth.sqlite")
	database, err := store.CreateGenesis(ctx, path, store.Options{Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	authority := &fakeAuthority{now: &now}
	external := &fakeWitness{now: &now}
	continuity := newTestContinuity(t)
	manager, err := NewManager(ManagerConfig{
		Store: database, Authority: authority, Witness: external, Continuity: continuity,
		Interval: 10 * time.Minute, MaxAge: 30 * time.Minute, Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Initialize(ctx); err != nil {
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	timestampCalls, witnessCount := authority.calls, len(external.history)

	inspection, err := store.InspectCurrent(ctx, path, store.Options{Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	verifier, err := NewManager(ManagerConfig{
		Store: inspection, Authority: authority, Witness: external, Continuity: continuity,
		Interval: 10 * time.Minute, MaxAge: 30 * time.Minute, Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := verifier.VerifyStartup(ctx); err != nil {
		t.Fatalf("read-only startup verification: %v", err)
	}
	if authority.calls != timestampCalls || len(external.history) != witnessCount {
		t.Fatalf("read-only verification caused publication: timestamp calls=%d/%d witnesses=%d/%d",
			authority.calls, timestampCalls, len(external.history), witnessCount)
	}
	if err := inspection.Close(); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, before) {
		t.Fatal("read-only startup verification changed SQLite bytes")
	}

	database, err = store.OpenCurrent(ctx, path, store.Options{Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = database.Close() }()
	restarted, err := NewManager(ManagerConfig{
		Store: database, Authority: authority, Witness: external, Continuity: continuity,
		Interval: 10 * time.Minute, MaxAge: 30 * time.Minute, Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := restarted.Initialize(ctx); err != nil {
		t.Fatalf("initialize after read-only verification: %v", err)
	}
}

func TestManagerAcceptsExactPendingGenesisWitnessBeforeFirstCheckpoint(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 8, 7, 10, 0, 0, 0, time.UTC)
	database, err := store.CreateGenesis(ctx, filepath.Join(t.TempDir(), "oberth.sqlite"), store.Options{Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = database.Close() }()
	action, err := database.AppendAuditAction(ctx, model.AuditActionSpec{
		Actor: "legacy@host", Action: "issue.create", ResourceType: "issue", ResourceID: "1", Details: `{"title":"legacy"}`,
	})
	if err != nil {
		t.Fatal(err)
	}
	continuity := newTestContinuity(t)
	if err := continuity.Prepare(ctx, model.AuditWitnessIntent{
		Sequence: 1, AuditID: action.ID, AuditSHA256: action.SHA256,
	}); err != nil {
		t.Fatal(err)
	}
	manager, err := NewManager(ManagerConfig{
		Store: database, Authority: &fakeAuthority{now: &now}, Witness: &fakeWitness{now: &now}, Continuity: continuity,
		Interval: 10 * time.Minute, MaxAge: 30 * time.Minute, Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.VerifyStartup(ctx); err != nil {
		t.Fatalf("verify exact pending first checkpoint: %v", err)
	}
}

func TestManagerAcceptsExactCompletedGenesisWitnessBeforeLocalCheckpoint(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 8, 7, 10, 1, 0, 0, time.UTC)
	database, err := store.CreateGenesis(ctx, filepath.Join(t.TempDir(), "oberth.sqlite"), store.Options{Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = database.Close() }()
	action, err := database.AppendAuditAction(ctx, model.AuditActionSpec{
		Actor: "legacy@host", Action: "issue.create", ResourceType: "issue", ResourceID: "1", Details: `{"title":"legacy"}`,
	})
	if err != nil {
		t.Fatal(err)
	}
	continuity := newTestContinuity(t)
	intent := model.AuditWitnessIntent{Sequence: 1, AuditID: action.ID, AuditSHA256: action.SHA256}
	if err := continuity.Prepare(ctx, intent); err != nil {
		t.Fatal(err)
	}
	external := &fakeWitness{now: &now}
	witness, err := external.Publish(ctx, model.AuditHead{ID: action.ID, SHA256: action.SHA256})
	if err != nil {
		t.Fatal(err)
	}
	if err := continuity.Reconcile(ctx, []model.AuditWitness{witness}); err != nil {
		t.Fatal(err)
	}
	manager, err := NewManager(ManagerConfig{
		Store: database, Authority: &fakeAuthority{now: &now}, Witness: external, Continuity: continuity,
		Interval: 10 * time.Minute, MaxAge: 30 * time.Minute, Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.VerifyStartup(ctx); err != nil {
		t.Fatalf("verify completed first witness before local checkpoint: %v", err)
	}
}

func TestManagerRejectsMultipleWitnessesWithoutLocalCheckpoint(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 8, 7, 10, 2, 0, 0, time.UTC)
	database, err := store.CreateGenesis(ctx, filepath.Join(t.TempDir(), "oberth.sqlite"), store.Options{Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = database.Close() }()
	continuity := newTestContinuity(t)
	external := &fakeWitness{now: &now}
	history := make([]model.AuditWitness, 0, 2)
	previousUUID := ""
	for sequence := 1; sequence <= 2; sequence++ {
		action, err := database.AppendAuditAction(ctx, model.AuditActionSpec{
			Actor: "legacy@host", Action: "issue.update", ResourceType: "issue",
			ResourceID: fmt.Sprintf("%d", sequence), Details: fmt.Sprintf(`{"sequence":%d}`, sequence),
		})
		if err != nil {
			t.Fatal(err)
		}
		intent := model.AuditWitnessIntent{
			Sequence: sequence, AuditID: action.ID, AuditSHA256: action.SHA256, PreviousUUID: previousUUID,
		}
		if err := continuity.Prepare(ctx, intent); err != nil {
			t.Fatal(err)
		}
		witness, err := external.Publish(ctx, model.AuditHead{ID: action.ID, SHA256: action.SHA256})
		if err != nil {
			t.Fatal(err)
		}
		history = append(history, witness)
		previousUUID = witness.UUID
	}
	if err := continuity.Reconcile(ctx, history); err != nil {
		t.Fatal(err)
	}
	manager, err := NewManager(ManagerConfig{
		Store: database, Authority: &fakeAuthority{now: &now}, Witness: external, Continuity: continuity,
		Interval: 10 * time.Minute, MaxAge: 30 * time.Minute, Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.VerifyStartup(ctx); err == nil || !strings.Contains(err.Error(), "without a signed checkpoint") {
		t.Fatalf("multiple witnesses without local checkpoint error = %v", err)
	}
}

func TestManagerKeepsValidCheckpointDuringTransientTSAFailure(t *testing.T) {
	now := time.Date(2026, 8, 6, 16, 0, 0, 0, time.UTC)
	database, err := store.OpenAdminClient(context.Background(), filepath.Join(t.TempDir(), "oberth.sqlite"), store.Options{Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = database.Close() }()
	authority := &fakeAuthority{now: &now}
	witness := &fakeWitness{now: &now}
	manager, err := NewManager(ManagerConfig{
		Store: database, Authority: authority, Witness: witness, Continuity: newTestContinuity(t), Interval: 10 * time.Minute,
		MaxAge: 30 * time.Minute, Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	manager.cycle(context.Background())
	now = now.Add(10 * time.Minute)
	authority.timestampErr = fmt.Errorf("%w: TSA offline", ErrAuthorityUnavailable)
	manager.cycle(context.Background())
	if err := manager.Ready(context.Background()); err != nil {
		t.Fatalf("fresh checkpoint was discarded after transient failure: %v", err)
	}
	now = now.Add(21 * time.Minute)
	manager.cycle(context.Background())
	if err := manager.Ready(context.Background()); err == nil {
		t.Fatal("TSA outage remained ready after the checkpoint became stale")
	}
}

func TestManagerDoesNotFallbackOnInvalidTimestampEvidence(t *testing.T) {
	now := time.Date(2026, 8, 6, 16, 15, 0, 0, time.UTC)
	database, err := store.OpenAdminClient(context.Background(), filepath.Join(t.TempDir(), "oberth.sqlite"), store.Options{Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = database.Close() }()
	authority := &fakeAuthority{now: &now}
	manager, err := NewManager(ManagerConfig{
		Store: database, Authority: authority, Witness: &fakeWitness{now: &now}, Continuity: newTestContinuity(t),
		Interval: 10 * time.Minute, MaxAge: 30 * time.Minute, Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	manager.cycle(context.Background())
	if err := manager.Ready(context.Background()); err != nil {
		t.Fatal(err)
	}
	authority.timestampErr = errors.New("invalid TSA signature")
	manager.cycle(context.Background())
	if err := manager.Ready(context.Background()); err == nil || !strings.Contains(err.Error(), "invalid TSA signature") {
		t.Fatalf("invalid timestamp evidence used cached fallback: %v", err)
	}
}

func TestMutationGateRechecksStateAfterCachedReadiness(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 8, 6, 16, 30, 0, 0, time.UTC)
	path := filepath.Join(t.TempDir(), "oberth.sqlite")
	database, err := store.OpenAdminClient(ctx, path, store.Options{Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = database.Close() }()
	if _, err := database.AppendAuditAction(ctx, model.AuditActionSpec{
		Actor: "agent@host", Action: "issue.create", ResourceType: "issue", ResourceID: "1",
	}); err != nil {
		t.Fatal(err)
	}
	manager, err := NewManager(ManagerConfig{
		Store: database, Authority: &fakeAuthority{now: &now}, Witness: &fakeWitness{now: &now}, Continuity: newTestContinuity(t),
		Interval: 10 * time.Minute, MaxAge: 30 * time.Minute, Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	manager.cycle(ctx)
	if err := manager.AllowMutation(ctx); err != nil {
		t.Fatalf("valid mutation gate: %v", err)
	}
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = raw.Close() }()
	if _, err := raw.ExecContext(ctx, `DROP TRIGGER audit_actions_no_update`); err != nil {
		t.Fatal(err)
	}
	if _, err := raw.ExecContext(ctx, `UPDATE audit_actions SET details = '{"tampered":true}' WHERE id = 1`); err != nil {
		t.Fatal(err)
	}
	if err := manager.Ready(ctx); err != nil {
		t.Fatalf("cached readiness unexpectedly refreshed itself: %v", err)
	}
	if err := manager.AllowMutation(ctx); err == nil {
		t.Fatal("mutation gate accepted state changed after the cached checkpoint")
	}
}

func TestManagerRequiresAnExternalWitness(t *testing.T) {
	now := time.Date(2026, 8, 6, 17, 0, 0, 0, time.UTC)
	database, err := store.OpenAdminClient(context.Background(), filepath.Join(t.TempDir(), "oberth.sqlite"), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = database.Close() }()
	if _, err := NewManager(ManagerConfig{
		Store: database, Authority: &fakeAuthority{now: &now}, Continuity: newTestContinuity(t), Interval: 10 * time.Minute, MaxAge: 30 * time.Minute,
	}); err == nil {
		t.Fatal("manager accepted SQLite-only audit anchoring")
	}
	if _, err := NewManager(ManagerConfig{
		Store: database, Authority: &fakeAuthority{now: &now}, Witness: &fakeWitness{now: &now},
		Interval: 10 * time.Minute, MaxAge: 30 * time.Minute,
	}); err == nil {
		t.Fatal("manager accepted rollbackable audit continuity")
	}
}

func TestManagerRejectsErasedHistoryAfterRestartEvenWithANewerWitness(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 8, 6, 18, 0, 0, 0, time.UTC)
	path := filepath.Join(t.TempDir(), "oberth.sqlite")
	database, err := store.OpenAdminClient(ctx, path, store.Options{Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.AppendAuditAction(ctx, model.AuditActionSpec{
		Actor: "agent@host", Action: "promotion.push", ResourceType: "promotion", ResourceID: "p-1",
	}); err != nil {
		t.Fatal(err)
	}
	authority := &fakeAuthority{now: &now}
	external := &fakeWitness{now: &now}
	continuity := newTestContinuity(t)
	manager, err := NewManager(ManagerConfig{
		Store: database, Authority: authority, Witness: external, Continuity: continuity, Interval: 10 * time.Minute,
		MaxAge: 30 * time.Minute, Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	manager.cycle(ctx)
	if err := manager.Ready(ctx); err != nil {
		t.Fatal(err)
	}
	if len(external.history) != 1 {
		t.Fatalf("external witness history = %#v", external.history)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}

	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		`DROP TRIGGER audit_actions_no_delete`,
		`DROP TRIGGER audit_anchors_no_delete`,
		`DELETE FROM audit_actions`,
		`DELETE FROM audit_anchors`,
	} {
		if _, err := raw.ExecContext(ctx, statement); err != nil {
			_ = raw.Close()
			t.Fatal(err)
		}
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}

	// A compromised local signer can append a newer genesis witness, but the
	// immutable older witness remains discoverable and must still be checked.
	external.history = append(external.history, model.AuditWitness{
		UUID: "witness-newer", LogIndex: 99, IntegratedAt: now.Add(time.Minute), AuditSHA256: make([]byte, 32),
	})
	database, err = store.OpenAdminClient(ctx, path, store.Options{Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = database.Close() }()
	restarted, err := NewManager(ManagerConfig{
		Store: database, Authority: authority, Witness: external, Continuity: continuity, Interval: 10 * time.Minute,
		MaxAge: 30 * time.Minute, Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	restarted.cycle(ctx)
	if err := restarted.Ready(ctx); err == nil {
		t.Fatal("erased pre-restart audit history passed external witness verification")
	}
}

func TestManagerRejectsWholeDatabaseRollbackWithStaleWitnessPrefix(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 8, 6, 18, 30, 0, 0, time.UTC)
	path := filepath.Join(t.TempDir(), "oberth.sqlite")
	database, err := store.OpenAdminClient(ctx, path, store.Options{Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.AppendAuditAction(ctx, model.AuditActionSpec{Actor: "agent@host", Action: "first", ResourceType: "test", ResourceID: "1"}); err != nil {
		t.Fatal(err)
	}
	authority := &fakeAuthority{now: &now}
	external := &fakeWitness{now: &now}
	continuity := newTestContinuity(t)
	manager, err := NewManager(ManagerConfig{
		Store: database, Authority: authority, Witness: external, Continuity: continuity, Interval: 10 * time.Minute,
		MaxAge: 30 * time.Minute, Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	manager.cycle(ctx)
	if err := manager.Ready(ctx); err != nil {
		t.Fatal(err)
	}
	staleAnchor, err := database.LatestAuditAnchor(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	databaseSnapshot, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	database, err = store.OpenAdminClient(ctx, path, store.Options{Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Minute)
	if _, err := database.AppendAuditAction(ctx, model.AuditActionSpec{Actor: "agent@host", Action: "second", ResourceType: "test", ResourceID: "2"}); err != nil {
		t.Fatal(err)
	}
	advanced, err := NewManager(ManagerConfig{
		Store: database, Authority: authority, Witness: external, Continuity: continuity, Interval: 10 * time.Minute,
		MaxAge: 30 * time.Minute, Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	advanced.cycle(ctx)
	if err := advanced.Ready(ctx); err != nil {
		t.Fatal(err)
	}
	if len(external.history) != 2 {
		t.Fatalf("external history = %#v", external.history)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}

	// Replace the complete SQLite file with the earlier snapshot. This rolls
	// back the audit chain, signed timestamps, migrations, and every other row
	// together, while the Kubernetes continuity suffix remains outside the PVC.
	if err := os.WriteFile(path, databaseSnapshot, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, suffix := range []string{"-wal", "-shm"} {
		if err := os.Remove(path + suffix); err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
	}
	database, err = store.OpenAdminClient(ctx, path, store.Options{Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	liveGate, err := NewManager(ManagerConfig{
		Store: database, Authority: authority, Witness: external, Continuity: continuity, Interval: 10 * time.Minute,
		MaxAge: 30 * time.Minute, Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	liveGate.setResult(staleAnchor, nil)
	if err := liveGate.AllowMutation(ctx); err == nil || !strings.Contains(err.Error(), "witness intent 2 references audit action 2 absent") {
		t.Fatalf("live mutation gate after whole-database rollback error = %v", err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	external.history = external.history[:1]
	beforeRejectedStartup, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	database, err = store.InspectCurrent(ctx, path, store.Options{Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	restarted, err := NewManager(ManagerConfig{
		Store: database, Authority: authority, Witness: external, Continuity: continuity, Interval: 10 * time.Minute,
		MaxAge: 30 * time.Minute, Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := restarted.VerifyStartup(ctx); err == nil || !strings.Contains(err.Error(), "witness intent 2 references audit action 2 absent") {
		t.Fatalf("whole-database rollback error = %v", err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	afterRejectedStartup, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(afterRejectedStartup, beforeRejectedStartup) {
		t.Fatal("rejected rolled-back startup changed SQLite bytes")
	}
}

func TestManagerRejectsRollbackAcrossPrePublicationIntentCrash(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 8, 6, 19, 15, 0, 0, time.UTC)
	path := filepath.Join(t.TempDir(), "oberth.sqlite")
	database, err := store.OpenAdminClient(ctx, path, store.Options{Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.AppendAuditAction(ctx, model.AuditActionSpec{Actor: "agent@host", Action: "first", ResourceType: "test", ResourceID: "1"}); err != nil {
		t.Fatal(err)
	}
	authority := &fakeAuthority{now: &now}
	external := &fakeWitness{now: &now}
	continuity := newTestContinuity(t)
	manager, err := NewManager(ManagerConfig{
		Store: database, Authority: authority, Witness: external, Continuity: continuity, Interval: 10 * time.Minute,
		MaxAge: 30 * time.Minute, Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	manager.cycle(ctx)
	if err := manager.Ready(ctx); err != nil {
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	snapshot, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	database, err = store.OpenAdminClient(ctx, path, store.Options{Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	second, err := database.AppendAuditAction(ctx, model.AuditActionSpec{Actor: "agent@host", Action: "second", ResourceType: "test", ResourceID: "2"})
	if err != nil {
		t.Fatal(err)
	}
	if err := continuity.Prepare(ctx, model.AuditWitnessIntent{
		Sequence: 2, AuditID: second.ID, AuditSHA256: second.SHA256, PreviousUUID: external.history[0].UUID,
	}); err != nil {
		t.Fatal(err)
	}
	// Simulate the exact crash window: the immutable intent exists, but Rekor
	// publication and the completed continuity record have not happened.
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, snapshot, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, suffix := range []string{"-wal", "-shm"} {
		if err := os.Remove(path + suffix); err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
	}
	database, err = store.OpenAdminClient(ctx, path, store.Options{Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = database.Close() }()
	restarted, err := NewManager(ManagerConfig{
		Store: database, Authority: authority, Witness: external, Continuity: continuity, Interval: 10 * time.Minute,
		MaxAge: 30 * time.Minute, Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	restarted.cycle(ctx)
	if err := restarted.Ready(ctx); err == nil || !strings.Contains(err.Error(), "witness intent 2 references audit action 2 absent") {
		t.Fatalf("pre-publication intent rollback error = %v", err)
	}
}

func TestManagerRecoversDegradedStartupWhenWitnessBecomeAvailable(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 8, 8, 10, 0, 0, 0, time.UTC)
	database, err := store.OpenAdminClient(ctx, filepath.Join(t.TempDir(), "oberth.sqlite"), store.Options{Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = database.Close() }()
	external := &fakeWitness{now: &now}
	external.historyErr = fmt.Errorf("%w: Rekor offline", ErrWitnessUnavailable)
	manager, err := NewManager(ManagerConfig{
		Store: database, Authority: &fakeAuthority{now: &now}, Witness: external, Continuity: newTestContinuity(t),
		Interval: 10 * time.Minute, MaxAge: 30 * time.Minute, Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	// Simulate degraded startup: Initialize fails because Rekor is unavailable.
	if err := manager.Initialize(ctx); !errors.Is(err, ErrWitnessUnavailable) {
		t.Fatalf("initialize during Rekor outage error = %v, want wrapped ErrWitnessUnavailable", err)
	}
	// The Manager's Run cycle will retry. Simulate Rekor becoming available.
	external.historyErr = nil
	manager.cycle(ctx)
	if err := manager.Ready(ctx); err != nil {
		t.Fatalf("readiness after Rekor recovery: %v", err)
	}
	if err := manager.AllowMutation(ctx); err != nil {
		t.Fatalf("mutation gate after Rekor recovery: %v", err)
	}
}

func TestManagerDegradedStartupBlocksMutationsUntilRecovery(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 8, 8, 10, 5, 0, 0, time.UTC)
	database, err := store.OpenAdminClient(ctx, filepath.Join(t.TempDir(), "oberth.sqlite"), store.Options{Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = database.Close() }()
	external := &fakeWitness{now: &now}
	external.historyErr = fmt.Errorf("%w: Rekor offline", ErrWitnessUnavailable)
	manager, err := NewManager(ManagerConfig{
		Store: database, Authority: &fakeAuthority{now: &now}, Witness: external, Continuity: newTestContinuity(t),
		Interval: 10 * time.Minute, MaxAge: 30 * time.Minute, Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	// Skip Initialize (degraded startup path in serve.go).
	// The Manager has never completed a cycle, so Ready and AllowMutation must fail.
	if err := manager.Ready(ctx); err == nil {
		t.Fatal("readiness should fail before any successful cycle")
	}
	if err := manager.AllowMutation(ctx); err == nil {
		t.Fatal("mutation gate should block before any successful cycle")
	}
	// Simulate Rekor recovery and a successful cycle.
	external.historyErr = nil
	manager.cycle(ctx)
	if err := manager.Ready(ctx); err != nil {
		t.Fatalf("readiness after recovery cycle: %v", err)
	}
	if err := manager.AllowMutation(ctx); err != nil {
		t.Fatalf("mutation gate after recovery cycle: %v", err)
	}
}
