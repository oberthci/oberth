package store

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"
)

func TestAuditFenceRejectsMovedHeadAndAllowsFreshRetry(t *testing.T) {
	now := time.Now()
	s := testStore(t, &now)
	calls := 0
	_, _, err := s.VerifyAuditMutationStateUnanchored(t.Context(), nil, nil, func(model.AuditHead, model.AuditAnchor) error {
		calls++
		_, err := s.AppendAuditAction(t.Context(), model.AuditActionSpec{Actor: "test", Action: "head.move", ResourceType: "test"})
		return err
	})
	if !errors.Is(err, ErrAuditStateChanged) || calls != 1 {
		t.Fatalf("moved-head gate = %v, callbacks=%d", err, calls)
	}
	head, _, err := s.VerifyAuditMutationStateUnanchored(t.Context(), nil, nil, acceptAuditMutationState)
	if err != nil || head.ID != 1 {
		t.Fatalf("fresh gate = %#v, %v", head, err)
	}
}

func TestAuditFenceRejectsChangesWithUnchangedHead(t *testing.T) {
	for _, change := range []string{"prefix", "receipt", "change-then-restore", "unrelated-write"} {
		t.Run(change, func(t *testing.T) {
			now := time.Now()
			s := testStore(t, &now)
			head := seedAuditMetricRows(t, s, 2, now)
			if _, err := s.RecordAuditAnchor(t.Context(), model.AuditAnchorSpec{AuditID: head.ID, AuditSHA256: head.SHA256, TSAURL: "https://tsa.example", Receipt: []byte("original"), AnchoredAt: now}); err != nil {
				t.Fatal(err)
			}
			for _, stmt := range []string{`DROP TRIGGER audit_actions_no_update`, `DROP TRIGGER audit_anchors_no_update`} {
				if _, err := s.db.ExecContext(t.Context(), stmt); err != nil {
					t.Fatal(err)
				}
			}
			witness := model.AuditWitness{UUID: "witness", AuditID: head.ID, AuditSHA256: head.SHA256, IntegratedAt: now}
			_, _, err := s.VerifyAuditMutationState(t.Context(), nil, []model.AuditWitness{witness}, func(model.AuditHead, model.AuditAnchor) error {
				switch change {
				case "prefix":
					_, err := s.db.ExecContext(t.Context(), `UPDATE audit_actions SET details='{"changed":true}' WHERE id=1`)
					return err
				case "receipt":
					_, err := s.db.ExecContext(t.Context(), `UPDATE audit_anchors SET receipt=?`, []byte("changed"))
					return err
				case "change-then-restore":
					var details string
					if err := s.db.QueryRowContext(t.Context(), `SELECT details FROM audit_actions WHERE id=1`).Scan(&details); err != nil {
						return err
					}
					if _, err := s.db.ExecContext(t.Context(), `UPDATE audit_actions SET details='{"changed":true}' WHERE id=1`); err != nil {
						return err
					}
					_, err := s.db.ExecContext(t.Context(), `UPDATE audit_actions SET details=? WHERE id=1`, details)
					return err
				default:
					_, err := s.db.ExecContext(t.Context(), `CREATE TABLE unrelated_audit_fence_test(id INTEGER)`)
					return err
				}
			})
			if !errors.Is(err, ErrAuditStateChanged) {
				t.Fatalf("%s accepted with unchanged head: %v", change, err)
			}
		})
	}
}

func TestAuditFenceRejectsPendingPublicationChange(t *testing.T) {
	now := time.Now()
	s := testStore(t, &now)
	repo := createRepo(t, s)
	run, err := s.EnqueueRun(t.Context(), testRunSpec(repo.ID, "feature/fence", strings.Repeat("b", 40)))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ClaimNextRun(t.Context()); err != nil {
		t.Fatal(err)
	}
	publication, err := s.BeginPublication(t.Context(), model.PublicationSpec{RepoID: repo.ID, RunID: run.ID, RefKind: model.RefBranch, Ref: run.Ref, ResultSHA: run.SHA, Actor: run.Actor})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(t.Context(), `DROP TRIGGER publications_guard_update`); err != nil {
		t.Fatal(err)
	}
	_, _, err = s.VerifyAuditMutationStateUnanchored(t.Context(), nil, nil, func(model.AuditHead, model.AuditAnchor) error {
		_, err := s.db.ExecContext(t.Context(), `UPDATE publications SET actor='forged' WHERE id=?`, publication.ID)
		return err
	})
	if !errors.Is(err, ErrAuditStateChanged) {
		t.Fatalf("pending-row change = %v", err)
	}
	invoked := false
	_, _, err = s.VerifyAuditMutationStateUnanchored(t.Context(), nil, nil, func(model.AuditHead, model.AuditAnchor) error { invoked = true; return nil })
	if err == nil || invoked {
		t.Fatalf("forged pending publication verified: callback=%t, err=%v", invoked, err)
	}
}

func TestAuditVerificationCPUDoesNotOwnPrimaryConnection(t *testing.T) {
	now := time.Now()
	s := testStore(t, &now)
	// Pause inside verification itself, with a real read-only snapshot open.
	// The primary connection and SQLite writer reservation must both be free.
	_, _, err := s.verifyAuditWithFence(t.Context(), func(tx *sql.Tx) (model.AuditHead, model.AuditAnchor, error) {
		head, anchor, _, err := s.verifyAuditMutationState(t.Context(), tx, nil, nil, false)
		if err != nil {
			return head, anchor, err
		}
		ctx, cancel := context.WithTimeout(t.Context(), time.Second)
		defer cancel()
		if _, err := s.AppendAuditAction(ctx, model.AuditActionSpec{Actor: "test", Action: "concurrent.writer", ResourceType: "test"}); err != nil {
			t.Fatalf("writer blocked during verification: %v", err)
		}
		return head, anchor, nil
	}, acceptAuditMutationState)
	if !errors.Is(err, ErrAuditStateChanged) {
		t.Fatalf("changed snapshot granted gate: %v", err)
	}
}

func TestLocalAuditGateRechecksPrefixAndHonorsCancellation(t *testing.T) {
	now := time.Now()
	s := testStore(t, &now)
	seedAuditMetricRows(t, s, 2, now)
	if _, _, err := s.VerifyLocalAuditMutationState(t.Context(), model.AuditHead{}, acceptAuditMutationState); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	_, _, err := s.VerifyLocalAuditMutationState(ctx, model.AuditHead{}, func(model.AuditHead, model.AuditAnchor) error { cancel(); return nil })
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled gate = %v", err)
	}
	for _, stmt := range []string{`DROP TRIGGER audit_actions_no_update`, `UPDATE audit_actions SET details='{"prefix":"forged"}' WHERE id=1`} {
		if _, err := s.db.ExecContext(t.Context(), stmt); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := s.VerifyLocalAuditMutationState(t.Context(), model.AuditHead{}, acceptAuditMutationState); err == nil || !strings.Contains(err.Error(), "audit action 1 hash is invalid") {
		t.Fatalf("prefix tamper = %v", err)
	}
}

func TestAuditFenceCallbackCannotMutateVerifiedResult(t *testing.T) {
	now := time.Now()
	s := testStore(t, &now)
	expected := seedAuditMetricRows(t, s, 1, now)
	got, _, err := s.VerifyLocalAuditMutationState(t.Context(), model.AuditHead{}, func(head model.AuditHead, _ model.AuditAnchor) error { clear(head.SHA256); return nil })
	if err != nil || !bytes.Equal(got.SHA256, expected.SHA256) {
		t.Fatalf("callback changed verified head: %#v, %v", got, err)
	}
}

func TestLocalAuditFencePreservesVerifiedPrefixPin(t *testing.T) {
	now := time.Now()
	s := testStore(t, &now)
	pinned := seedAuditMetricRows(t, s, 2, now)
	if _, _, err := s.VerifyLocalAuditMutationState(t.Context(), pinned, acceptAuditMutationState); err != nil {
		t.Fatal(err)
	}
	for _, stmt := range []string{`DROP TRIGGER audit_actions_no_delete`, `DELETE FROM audit_actions WHERE id=2`} {
		if _, err := s.db.ExecContext(t.Context(), stmt); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.VerifyAuditChain(t.Context()); err != nil {
		t.Fatalf("test needs a valid shortened chain: %v", err)
	}
	if _, _, err := s.VerifyLocalAuditMutationState(t.Context(), pinned, acceptAuditMutationState); err == nil || !strings.Contains(err.Error(), "verified audit prefix") {
		t.Fatalf("shortened chain bypassed prefix pin: %v", err)
	}
}
