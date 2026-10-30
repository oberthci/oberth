package store

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/testtimeout"
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
	if !errors.Is(err, ErrAuditVerificationBusy) || calls != 1 {
		t.Fatalf("moved-head gate = %v, callbacks=%d", err, calls)
	}
	head, _, err := s.VerifyAuditMutationStateUnanchored(t.Context(), nil, nil, acceptAuditMutationState)
	if err != nil || head.ID != 1 {
		t.Fatalf("fresh gate = %#v, %v", head, err)
	}
}

func TestAuditFenceReverifiesChangesWithUnchangedHead(t *testing.T) {
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
			switch change {
			case "prefix":
				if err == nil || !strings.Contains(err.Error(), "audit action 1 hash is invalid") {
					t.Fatalf("prefix not rejected by re-verification: %v", err)
				}
			case "receipt":
				if !errors.Is(err, ErrAuditStateChanged) || errors.Is(err, ErrAuditVerificationBusy) || !strings.Contains(err.Error(), "checkpoint differs") {
					t.Fatalf("changed external proof accepted: %v", err)
				}
			default:
				if err != nil {
					t.Fatalf("unchanged protected state refused after %s: %v", change, err)
				}
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
	if err == nil || !strings.Contains(err.Error(), "does not match its latest chained audit action") {
		t.Fatalf("pending-row change not rejected by re-verification: %v", err)
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
	_, _, err := s.verifyAuditWithFence(t.Context(), nil, func(tx *sql.Tx) (model.AuditHead, model.AuditAnchor, error) {
		head, anchor, _, err := s.verifyAuditMutationState(t.Context(), tx, nil, nil, false)
		if err != nil {
			return head, anchor, err
		}
		ctx, cancel := context.WithTimeout(t.Context(), testtimeout.Start(time.Second))
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

func TestAuditFenceOutsidePrefixEditWithRestoredSchemaFailsClosed(t *testing.T) {
	now := time.Now()
	s := testStore(t, &now)
	seedAuditMetricRows(t, s, 3, now)
	_, _, err := s.VerifyAuditMutationStateUnanchored(t.Context(), nil, nil, func(model.AuditHead, model.AuditAnchor) error {
		outsideAuditEdit(t, s, `UPDATE audit_actions SET actor='forged' WHERE id=1`)
		return nil
	})
	if err == nil || !strings.Contains(err.Error(), "audit action 1 hash is invalid") || errors.Is(err, ErrAuditVerificationBusy) {
		t.Fatalf("outside prefix edit accepted/retried: %v", err)
	}
}

func TestAuditFenceFileReplacementDuringCallbackFailsClosed(t *testing.T) {
	for _, mode := range []string{"local", "witnessed"} {
		t.Run(mode, func(t *testing.T) {
			now := time.Now()
			s := testStore(t, &now)
			seedAuditMetricRows(t, s, 3, now)
			verify := func(callback func(model.AuditHead, model.AuditAnchor) error) error {
				if mode == "local" {
					_, _, err := s.VerifyLocalAuditMutationState(t.Context(), model.AuditHead{}, callback)
					return err
				}
				_, _, err := s.VerifyAuditMutationStateUnanchored(t.Context(), nil, nil, callback)
				return err
			}
			err := verify(func(model.AuditHead, model.AuditAnchor) error {
				if err := os.Rename(s.livePath, s.livePath+".original"); err != nil {
					return err
				}
				t.Cleanup(func() {
					if err := os.Rename(s.livePath+".original", s.livePath); err != nil {
						t.Error(err)
					}
				})
				return os.WriteFile(s.livePath, nil, 0600)
			})
			if !errors.Is(err, ErrAuditStateChanged) || errors.Is(err, ErrAuditVerificationBusy) || !strings.Contains(err.Error(), "file replaced") {
				t.Fatalf("file replacement accepted/retried: %v", err)
			}
		})
	}
}

func TestAuditFenceRetriesCleanForeignCommitBursts(t *testing.T) {
	for _, commits := range []int{1, auditFenceExtraRounds + 1, 2 * (auditFenceExtraRounds + 1)} {
		t.Run(fmt.Sprintf("commits=%d", commits), func(t *testing.T) {
			now := time.Now()
			s := testStore(t, &now)
			credential, err := s.CreateTokenCredential(t.Context(), model.TokenCredentialSpec{Name: "outside", Digest: make([]byte, 32)})
			if err != nil {
				t.Fatal(err)
			}
			outside, err := sql.Open("sqlite", s.livePath)
			if err != nil {
				t.Fatal(err)
			}
			defer outside.Close()
			snapshots, external, writes, busy := 0, 0, 0, 0
			ctx, cancel := context.WithTimeout(t.Context(), testtimeout.Start(5*time.Second))
			defer cancel()
			for {
				before := snapshots
				_, _, err = s.verifyAuditWithFence(ctx, nil, func(tx *sql.Tx) (model.AuditHead, model.AuditAnchor, error) {
					snapshots++
					head, anchor, _, err := s.verifyAuditMutationState(ctx, tx, nil, nil, false)
					if err == nil && writes < commits {
						_, err = outside.ExecContext(ctx, `UPDATE token_credentials SET last_used_at=COALESCE(last_used_at,0)+1 WHERE id=?`, credential.ID)
						if err == nil {
							writes++
						}
					}
					return head, anchor, err
				}, func(model.AuditHead, model.AuditAnchor) error { external++; return nil })
				if err == nil {
					break
				}
				if !errors.Is(err, ErrAuditVerificationBusy) || !errors.Is(err, ErrAuditStateChanged) || !strings.Contains(err.Error(), "rounds exhausted") || snapshots-before != auditFenceExtraRounds+1 {
					t.Fatalf("clean commits not transient: err=%v snapshots=%d external=%d", err, snapshots, external)
				}
				busy++
			}
			wantBusy := commits / (auditFenceExtraRounds + 1)
			if snapshots != commits+1 || writes != commits || busy != wantBusy || external != wantBusy+1 {
				t.Fatalf("snapshots=%d writes=%d busy=%d external=%d", snapshots, writes, busy, external)
			}
			// A single foreign commit is absorbed locally with one extra snapshot.
			// Longer bursts exceed the bound and succeed only after a fresh attempt.
			t.Logf("foreign commits=%d snapshots=%d busy=%d external=%d", writes, snapshots, busy, external)
		})
	}
}

func TestAuditFenceForeignTamperFailsAtFirstFailedRound(t *testing.T) {
	for _, tamperAt := range []int{1, auditFenceExtraRounds + 1} {
		t.Run(fmt.Sprintf("tamper-round=%d", tamperAt), func(t *testing.T) {
			now := time.Now()
			s := testStore(t, &now)
			seedAuditMetricRows(t, s, 3, now)
			snapshots, external, attempts := 0, 0, 0
			for {
				attempts++
				_, _, err := s.verifyAuditWithFence(t.Context(), nil, func(tx *sql.Tx) (model.AuditHead, model.AuditAnchor, error) {
					snapshots++
					head, anchor, _, err := s.verifyAuditMutationState(t.Context(), tx, nil, nil, false)
					if err != nil {
						return head, anchor, err
					}
					statement := `UPDATE audit_actions SET actor=actor WHERE id=1`
					if snapshots == tamperAt {
						statement = `UPDATE audit_actions SET actor='forged' WHERE id=1`
					}
					outsideAuditEdit(t, s, statement)
					return head, anchor, nil
				}, func(model.AuditHead, model.AuditAnchor) error { external++; return nil })
				if errors.Is(err, ErrAuditVerificationBusy) && attempts == 1 && tamperAt == auditFenceExtraRounds+1 {
					continue
				}
				if err == nil || errors.Is(err, ErrAuditVerificationBusy) || !strings.Contains(err.Error(), "audit action 1 hash is invalid") || snapshots != tamperAt+1 || external != 1 {
					t.Fatalf("tampering not stopped at first failed round: err=%v snapshots=%d external=%d attempts=%d", err, snapshots, external, attempts)
				}
				break
			}
		})
	}
}

func TestAuditFenceRejectsTrustedCheckpointAppendWithIntactGuards(t *testing.T) {
	now := time.Now()
	s := testStore(t, &now)
	seedAuditMetricRows(t, s, 3, now)
	snapshots := 0
	_, _, err := s.verifyAuditWithFence(t.Context(), nil, func(tx *sql.Tx) (model.AuditHead, model.AuditAnchor, error) {
		snapshots++
		head, anchor, _, err := s.verifyAuditMutationState(t.Context(), tx, nil, nil, false)
		return head, anchor, err
	}, func(head model.AuditHead, _ model.AuditAnchor) error {
		_, err := s.RecordAuditAnchor(t.Context(), model.AuditAnchorSpec{AuditID: head.ID, AuditSHA256: head.SHA256, TSAURL: "https://tsa.example", Receipt: []byte("new checkpoint"), AnchoredAt: now})
		return err
	})
	if !errors.Is(err, ErrAuditStateChanged) || errors.Is(err, ErrAuditVerificationBusy) || !strings.Contains(err.Error(), "checkpoint differs") || snapshots != 1 {
		t.Fatalf("checkpoint append accepted/retried: err=%v snapshots=%d", err, snapshots)
	}
}

func TestAuditFenceRejectsRegressedOrReplacedHeadWithoutBusyRetry(t *testing.T) {
	for _, writer := range []string{"primary", "foreign"} {
		for _, change := range []string{"regressed", "replaced", "replaced-and-advanced"} {
			t.Run(writer+"/"+change, func(t *testing.T) {
				now := time.Now()
				s := testStore(t, &now)
				head := seedAuditMetricRows(t, s, 3, now)
				writeStore := s
				if writer == "foreign" {
					var err error
					writeStore, err = OpenAdminClient(t.Context(), s.livePath, Options{})
					if err != nil {
						t.Fatal(err)
					}
					defer writeStore.Close()
				}
				_, _, err := s.VerifyAuditMutationStateUnanchored(t.Context(), nil, nil, func(model.AuditHead, model.AuditAnchor) error {
					switch change {
					case "regressed":
						_, err := writeStore.db.ExecContext(t.Context(), `DROP TRIGGER audit_actions_no_delete; DELETE FROM audit_actions WHERE id=3`)
						return err
					default:
						if change == "replaced-and-advanced" {
							if _, err := writeStore.AppendAuditAction(t.Context(), model.AuditActionSpec{Actor: "outside", Action: "append.before.replacement", ResourceType: "test"}); err != nil {
								return err
							}
						}
						_, err := writeStore.db.ExecContext(t.Context(), `DROP TRIGGER audit_actions_no_update; UPDATE audit_actions SET sha256=? WHERE id=?`, make([]byte, 32), head.ID)
						return err
					}
				})
				if !errors.Is(err, ErrAuditStateChanged) || errors.Is(err, ErrAuditVerificationBusy) {
					t.Fatalf("%s head accepted/retried: %v", change, err)
				}
			})
		}
	}
}
