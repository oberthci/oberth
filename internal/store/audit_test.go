package store

import (
	"bytes"
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"
)

type auditHistoricalAnchorQueryGuard struct {
	auditStateQuerier
	loadedReceipts bool
}

func (guard *auditHistoricalAnchorQueryGuard) QueryContext(
	ctx context.Context,
	query string,
	args ...any,
) (*sql.Rows, error) {
	if strings.Contains(query, "FROM audit_anchors ORDER BY id") && strings.Contains(query, "receipt") {
		guard.loadedReceipts = true
	}
	return guard.auditStateQuerier.QueryContext(ctx, query, args...)
}

func acceptAuditMutationState(model.AuditHead, model.AuditAnchor) error {
	return nil
}

func TestAuditChainRejectsHistoricalMutation(t *testing.T) {
	now := time.Date(2026, 8, 6, 12, 0, 0, 0, time.UTC)
	store := testStore(t, &now)
	ctx := context.Background()
	for _, action := range []string{"push.branch.queued", "promotion.admit", "promotion.push.outcome"} {
		if _, err := store.AppendAuditAction(ctx, model.AuditActionSpec{
			Actor: "agent@tuxbox", Action: action, ResourceType: "test", ResourceID: action,
		}); err != nil {
			t.Fatal(err)
		}
		now = now.Add(time.Second)
	}
	head, err := store.VerifyAuditChain(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if head.ID != 3 || len(head.SHA256) != 32 {
		t.Fatalf("audit head = %#v", head)
	}
	if _, err := store.db.ExecContext(ctx, `DROP TRIGGER audit_actions_no_update`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `UPDATE audit_actions SET details = '{"rewritten":true}' WHERE id = 2`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.VerifyAuditChain(ctx); err == nil {
		t.Fatal("rewritten audit history passed chain verification")
	}
}

func TestExternalAnchorDetectsRecomputedHistory(t *testing.T) {
	now := time.Date(2026, 8, 6, 13, 0, 0, 0, time.UTC)
	store := testStore(t, &now)
	ctx := context.Background()
	firstSpec := model.AuditActionSpec{Actor: "agent@tuxbox", Action: "first", ResourceType: "test", ResourceID: "1", Details: `{}`}
	secondSpec := model.AuditActionSpec{Actor: "agent@tuxbox", Action: "second", ResourceType: "test", ResourceID: "2", Details: `{}`}
	first, err := store.AppendAuditAction(ctx, firstSpec)
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Second)
	second, err := store.AppendAuditAction(ctx, secondSpec)
	if err != nil {
		t.Fatal(err)
	}
	anchor, err := store.RecordAuditAnchor(ctx, model.AuditAnchorSpec{
		AuditID: second.ID, AuditSHA256: second.SHA256, TSAURL: "https://tsa.example.test",
		Receipt: []byte("externally signed receipt"), AnchoredAt: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.VerifyAuditAnchorReference(ctx, anchor); err != nil {
		t.Fatal(err)
	}
	witnesses := []model.AuditWitness{{
		UUID: "external-log-entry", LogIndex: 42, IntegratedAt: now, AuditID: second.ID,
		AuditSHA256: append([]byte(nil), second.SHA256...),
	}}
	if err := store.VerifyAuditWitnesses(ctx, witnesses); err != nil {
		t.Fatalf("valid external witness history: %v", err)
	}

	// Model a database-root attacker who disables the local trigger and then
	// recomputes every hash after changing the first historical action.
	if _, err := store.db.ExecContext(ctx, `DROP TRIGGER audit_actions_no_update`); err != nil {
		t.Fatal(err)
	}
	firstSpec.Details = `{"rewritten":true}`
	rewrittenFirst := auditActionSHA256(first.ID, auditGenesisSHA256, firstSpec, first.CreatedAt.UnixNano())
	rewrittenSecond := auditActionSHA256(second.ID, rewrittenFirst, secondSpec, second.CreatedAt.UnixNano())
	if _, err := store.db.ExecContext(ctx, `
UPDATE audit_actions SET details = ?, previous_sha256 = ?, sha256 = ? WHERE id = ?`,
		firstSpec.Details, auditGenesisSHA256, rewrittenFirst, first.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `
UPDATE audit_actions SET previous_sha256 = ?, sha256 = ? WHERE id = ?`, rewrittenFirst, rewrittenSecond, second.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.VerifyAuditChain(ctx); err != nil {
		t.Fatalf("attacker's recomputed local chain should be internally consistent: %v", err)
	}
	if err := store.VerifyAuditAnchorReference(ctx, anchor); err == nil {
		t.Fatal("recomputed history still matched the independently signed checkpoint")
	}
	if err := store.VerifyAuditWitnesses(ctx, witnesses); err == nil {
		t.Fatal("recomputed history still contained the externally witnessed head")
	}
	latest, err := store.LatestAuditAnchor(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if latest.ID != anchor.ID || !bytes.Equal(latest.Receipt, anchor.Receipt) {
		t.Fatalf("latest anchor = %#v, want %#v", latest, anchor)
	}
	if _, err := store.db.ExecContext(ctx, `UPDATE audit_anchors SET receipt = X'00' WHERE id = ?`, anchor.ID); err == nil {
		t.Fatal("audit anchor update unexpectedly succeeded")
	}
	if _, err := store.db.ExecContext(ctx, `DELETE FROM audit_anchors WHERE id = ?`, anchor.ID); err == nil {
		t.Fatal("audit anchor delete unexpectedly succeeded")
	}
}

func TestAuditMutationStateUsesOneAuthoritativeAuditPass(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 8, 6, 20, 30, 0, 0, time.UTC)
	database := testStore(t, &now)
	head := seedAuditMetricRows(t, database, 1000, now)
	anchor, err := database.RecordAuditAnchor(ctx, model.AuditAnchorSpec{
		AuditID: head.ID, AuditSHA256: head.SHA256, TSAURL: "https://tsa.example.test",
		Receipt: []byte("valid receipt"), AnchoredAt: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	intents := []model.AuditWitnessIntent{{Sequence: 1, AuditID: head.ID, AuditSHA256: append([]byte(nil), head.SHA256...)}}
	witnesses := []model.AuditWitness{{
		UUID: "witness-1", LogIndex: 0, IntegratedAt: now,
		AuditID: head.ID, AuditSHA256: append([]byte(nil), head.SHA256...),
	}}
	tx, err := database.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	guard := &auditHistoricalAnchorQueryGuard{auditStateQuerier: tx}
	verifiedHead, verifiedAnchor, metrics, err := verifyAuditMutationState(ctx, guard, intents, witnesses)
	if err != nil {
		t.Fatal(err)
	}
	if metrics.fullTraversals != 1 || metrics.readStatements != 4 {
		t.Fatalf("audit mutation verification metrics = %#v, want one traversal and four reads", metrics)
	}
	if guard.loadedReceipts {
		t.Fatal("audit mutation verification loaded receipt blobs for historical anchors")
	}
	if verifiedHead.ID != head.ID || !bytes.Equal(verifiedHead.SHA256, head.SHA256) || !sameMetricAnchor(verifiedAnchor, anchor) {
		t.Fatalf("verified state = (%#v, %#v), want (%#v, %#v)", verifiedHead, verifiedAnchor, head, anchor)
	}
}

func TestAuditMutationStateRejectsExternalReferenceMismatch(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 8, 6, 20, 45, 0, 0, time.UTC)
	for _, test := range []struct {
		name        string
		mutate      func(*testing.T, *Store)
		mutateData  func(*model.AuditWitnessIntent, *model.AuditWitness)
		omitWitness bool
		want        string
	}{
		{
			name: "intent hash", want: "witness intent 1 references audit action 2 absent",
			mutateData: func(intent *model.AuditWitnessIntent, _ *model.AuditWitness) {
				intent.AuditSHA256[0] ^= 0xff
			},
		},
		{
			name: "witness hash", want: "external audit witness witness-1 references audit action 2 absent",
			mutateData: func(_ *model.AuditWitnessIntent, witness *model.AuditWitness) {
				witness.AuditSHA256[0] ^= 0xff
			},
		},
		{
			name: "anchor id", want: "audit anchor hash does not match action 1",
			mutate: func(t *testing.T, database *Store) {
				t.Helper()
				if _, err := database.db.ExecContext(ctx, `DROP TRIGGER audit_anchors_no_update`); err != nil {
					t.Fatal(err)
				}
				if _, err := database.db.ExecContext(ctx, `UPDATE audit_anchors SET audit_id = 1`); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "anchor without witness", omitWitness: true,
			want: "local audit anchor 1 has no recoverable external witness",
		},
		{
			name: "malformed intent sequence", want: "external audit witness intent 1 is invalid",
			mutateData: func(intent *model.AuditWitnessIntent, _ *model.AuditWitness) {
				intent.Sequence = 2
			},
		},
		{
			name: "malformed witness", want: "external audit witness fields are invalid",
			mutateData: func(_ *model.AuditWitnessIntent, witness *model.AuditWitness) {
				witness.UUID = ""
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			database := testStore(t, &now)
			head := seedAuditMetricRows(t, database, 2, now)
			if _, err := database.RecordAuditAnchor(ctx, model.AuditAnchorSpec{
				AuditID: head.ID, AuditSHA256: head.SHA256, TSAURL: "https://tsa.example.test",
				Receipt: []byte("valid receipt"), AnchoredAt: now,
			}); err != nil {
				t.Fatal(err)
			}
			intent := model.AuditWitnessIntent{Sequence: 1, AuditID: head.ID, AuditSHA256: append([]byte(nil), head.SHA256...)}
			witness := model.AuditWitness{
				UUID: "witness-1", LogIndex: 0, IntegratedAt: now,
				AuditID: head.ID, AuditSHA256: append([]byte(nil), head.SHA256...),
			}
			if test.mutate != nil {
				test.mutate(t, database)
			}
			if test.mutateData != nil {
				test.mutateData(&intent, &witness)
			}
			witnesses := []model.AuditWitness{witness}
			if test.omitWitness {
				witnesses = nil
			}
			if _, _, err := database.VerifyAuditMutationState(
				ctx,
				[]model.AuditWitnessIntent{intent},
				witnesses,
				acceptAuditMutationState,
			); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("VerifyAuditMutationState error = %v, want %q", err, test.want)
			}
		})
	}
}
