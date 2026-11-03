package store

import (
	"errors"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/vmrunner"
)

func reserveRestartPreflight(t *testing.T, s *Store, intent vmrunner.KVMIntent, clean bool) {
	t.Helper()
	_, err := s.ReserveKVMPreflight(t.Context(), intent)
	requireStoreNoError(t, err)
	requireStoreNoError(t, s.SubmitKVMPreflight(t.Context(), intent.RunID))
	requireStoreNoError(t, s.BindKVMPreflight(t.Context(), intent.RunID, "workflow-uid"))
	if clean {
		requireStoreNoError(t, s.RecordKVMRefusal(t.Context(), intent.RunID, vmrunner.KVMRefusal{Code: "canceled", ObservedAt: s.now()}))
		requireStoreNoError(t, s.CompleteKVMPreflightCleanup(t.Context(), intent.RunID, "workflow-uid"))
	}
}

func TestKVMRestartDefersOwnerRequeueUntilCleanupAndRetriesOnce(t *testing.T) {
	now := time.Now().UTC()
	s := testStore(t, &now)
	intent := kvmIntent(t, s)
	requireStoreNoError(t, s.SetRunCredentialed(t.Context(), intent.RunID))
	reserveRestartPreflight(t, s, intent, false)
	source, err := s.Run(t.Context(), intent.RunID)
	requireStoreNoError(t, err)
	requireStoreNoError(t, s.RecoverOwnerState(t.Context()))
	original, err := s.Run(t.Context(), source.ID)
	requireStoreNoError(t, err)
	if original.Status != model.RunRunning || original.SupersededBy != "" {
		t.Fatalf("owner recovery bypassed preflight cleanup: %+v", original)
	}
	if _, err := s.RecoverKVMPreflight(t.Context(), source.ID); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("unclean preflight retried: %v", err)
	}
	requireStoreNoError(t, s.RecordKVMRefusal(t.Context(), source.ID, vmrunner.KVMRefusal{Code: "canceled", ObservedAt: now}))
	requireStoreNoError(t, s.CompleteKVMPreflightCleanup(t.Context(), source.ID, "workflow-uid"))
	next, err := s.RecoverKVMPreflight(t.Context(), source.ID)
	requireStoreNoError(t, err)
	if next.ID == "" || next.Status != model.RunQueued || next.AdmissionSequence != source.AdmissionSequence || next.SHA != source.SHA || next.Actor != source.Actor || next.Credentialed {
		t.Fatalf("invalid replacement: %+v", next)
	}
	original, err = s.Run(t.Context(), source.ID)
	requireStoreNoError(t, err)
	if original.Status != model.RunInterrupted || original.SupersededBy != next.ID || original.JobName != "" {
		t.Fatalf("parent not cleanly interrupted: %+v", original)
	}
	if duplicate, err := s.RecoverKVMPreflight(t.Context(), source.ID); err != nil || duplicate.ID != "" {
		t.Fatalf("settlement not idempotent: %+v %v", duplicate, err)
	}
	// Even a restart before the replacement reserves its VM preserves the cap.
	claimed, err := s.ClaimNextRun(t.Context())
	requireStoreNoError(t, err)
	if claimed.ID != next.ID {
		t.Fatal("wrong retry claimed")
	}
	requireStoreNoError(t, s.RecoverOwnerState(t.Context()))
	claimed, err = s.ClaimNextRun(t.Context())
	requireStoreNoError(t, err)
	if claimed.ID == next.ID || claimed.AdmissionSequence != source.AdmissionSequence {
		t.Fatalf("restart did not inherit order: %+v", claimed)
	}
	intent.RunID = claimed.ID
	intent.WorkflowName = "trusted-" + claimed.ID
	reserveRestartPreflight(t, s, intent, true)
	last, err := s.RecoverKVMPreflight(t.Context(), claimed.ID)
	requireStoreNoError(t, err)
	if last.ID != "" {
		t.Fatalf("automatic retry loop: %+v", last)
	}
	claimed, err = s.Run(t.Context(), claimed.ID)
	requireStoreNoError(t, err)
	if claimed.Status != model.RunInterrupted {
		t.Fatalf("exhausted retry not interrupted: %+v", claimed)
	}
	_, err = s.VerifyAuditState(t.Context())
	requireStoreNoError(t, err)
}

func TestKVMRestartRetrySurvivesReopenAndDoesNotOverwritePositiveReceipt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "restart.sqlite")
	s, err := Open(t.Context(), path, Options{})
	requireStoreNoError(t, err)
	intent := kvmIntent(t, s)
	reserveRestartPreflight(t, s, intent, false)
	requireStoreNoError(t, s.RecordKVMReceipt(t.Context(), intent.RunID, kvmReceipt(intent)))
	requireStoreNoError(t, s.CompleteKVMPreflightCleanup(t.Context(), intent.RunID, "workflow-uid"))
	requireStoreNoError(t, s.Close())
	s, err = Open(t.Context(), path, Options{})
	requireStoreNoError(t, err)
	defer s.Close()
	requireStoreNoError(t, s.RecoverOwnerState(t.Context()))
	next, err := s.RecoverKVMPreflight(t.Context(), intent.RunID)
	requireStoreNoError(t, err)
	if next.ID == "" {
		t.Fatal("no retry after process reopen")
	}
	state, err := s.KVMPreflight(t.Context(), intent.RunID)
	requireStoreNoError(t, err)
	if state.Receipt == nil || state.Refusal != nil {
		t.Fatal("recovery replaced immutable evidence")
	}
	claimed, err := s.ClaimNextRun(t.Context())
	requireStoreNoError(t, err)
	intent.RunID, intent.WorkflowName = claimed.ID, "trusted-"+claimed.ID
	intent.AdmittedAt = s.now()
	intent.Deadline = intent.AdmittedAt.Add(time.Minute)
	reserveRestartPreflight(t, s, intent, true)
	requireStoreNoError(t, s.Close())
	s, err = Open(t.Context(), path, Options{})
	requireStoreNoError(t, err)
	defer s.Close()
	requireStoreNoError(t, s.RecoverOwnerState(t.Context()))
	if duplicate, err := s.RecoverKVMPreflight(t.Context(), intent.RunID); err != nil || duplicate.ID != "" {
		t.Fatalf("durable retry cap lost: %+v %v", duplicate, err)
	}
}

func TestKVMRestartNeverRetriesCanceledSupersededOrNewerWork(t *testing.T) {
	for _, mode := range []string{"canceled", "newer queued", "newer finished", "inactive", "main Job", "publication"} {
		t.Run(mode, func(t *testing.T) {
			now := time.Now().UTC()
			s := testStore(t, &now)
			intent := kvmIntent(t, s)
			reserveRestartPreflight(t, s, intent, true)
			source, err := s.Run(t.Context(), intent.RunID)
			requireStoreNoError(t, err)
			switch mode {
			case "canceled":
				_, err = s.CancelRun(t.Context(), model.RunCancelRequest{RunID: source.ID, Actor: source.Actor})
				requireStoreNoError(t, err)
			case "newer queued", "newer finished":
				_, err = s.EnqueueRun(t.Context(), model.RunSpec{RepoID: source.RepoID, RefKind: source.RefKind, Ref: source.Ref, SHA: strings.Repeat("9", 40), Actor: source.Actor, Trigger: source.Trigger})
				requireStoreNoError(t, err)
				if mode == "newer finished" {
					run, e := s.ClaimNextRun(t.Context())
					requireStoreNoError(t, e)
					_, e = s.FinishRun(t.Context(), run.ID, model.RunResult{Status: model.RunPassed})
					requireStoreNoError(t, e)
				}
			case "inactive":
				_, err = s.AppendAuditAction(t.Context(), model.AuditActionSpec{Actor: "admin", Action: "repository.archive", ResourceType: "repository", ResourceID: strconv.FormatInt(source.RepoID, 10), Details: "{}"})
				requireStoreNoError(t, err)
			case "main Job":
				_, err = s.db.Exec(`UPDATE runs SET job_name='main-job' WHERE id=?`, source.ID)
				requireStoreNoError(t, err)
			case "publication":
				_, err = s.BeginPublication(t.Context(), model.PublicationSpec{RepoID: source.RepoID, RunID: source.ID, RefKind: source.RefKind, Ref: source.Ref, ResultSHA: source.SHA, PreviousKnown: true, Actor: source.Actor})
				requireStoreNoError(t, err)

			}
			next, err := s.RecoverKVMPreflight(t.Context(), source.ID)
			if mode == "main Job" || mode == "publication" {
				if !errors.Is(err, ErrInvalidState) {
					t.Fatal("delivered parent recovered")
				}
				return
			}
			requireStoreNoError(t, err)
			if next.ID != "" {
				t.Fatalf("replayed %s: %+v", mode, next)
			}
		})
	}
}

func TestKVMRestartReleaseRetestsBeforeAnyPublication(t *testing.T) {
	now := time.Now().UTC()
	s := testStore(t, &now)
	intent := kvmIntent(t, s)
	_, err := s.db.Exec(`UPDATE runs SET ref_kind='tag',ref='v1',release=1,credentialed=1,trigger='tag' WHERE id=?`, intent.RunID)
	requireStoreNoError(t, err)
	reserveRestartPreflight(t, s, intent, true)
	next, err := s.RecoverKVMPreflight(t.Context(), intent.RunID)
	requireStoreNoError(t, err)
	if next.ID == "" || !next.Release || !next.Credentialed || next.Trigger != "tag" || next.JobName != "" {
		t.Fatalf("release identity lost: %+v", next)
	}
	if _, err = s.KVMPreflight(t.Context(), next.ID); !errors.Is(err, ErrNotFound) {
		t.Fatal("old receipt transferred to replacement")
	}
	var publications int
	requireStoreNoError(t, s.db.QueryRow(`SELECT COUNT(*) FROM publications`).Scan(&publications))
	if publications != 0 {
		t.Fatal("restart published without new evidence")
	}
}

func TestKVMRestartSettlementRollsBackWithAuditFailure(t *testing.T) {
	now := time.Now().UTC()
	s := testStore(t, &now)
	intent := kvmIntent(t, s)
	reserveRestartPreflight(t, s, intent, true)
	_, err := s.db.Exec(`CREATE TRIGGER reject_restart_audit BEFORE INSERT ON audit_actions WHEN NEW.action='kvm.restart-retry' BEGIN SELECT RAISE(FAIL,'injected audit failure'); END`)
	requireStoreNoError(t, err)
	if _, err = s.RecoverKVMPreflight(t.Context(), intent.RunID); err == nil {
		t.Fatal("audit failure committed retry")
	}
	run, err := s.Run(t.Context(), intent.RunID)
	requireStoreNoError(t, err)
	if run.Status != model.RunRunning || run.SupersededBy != "" {
		t.Fatal("partial interruption committed without retry ledger")
	}
	var runs int
	requireStoreNoError(t, s.db.QueryRow(`SELECT COUNT(*) FROM runs`).Scan(&runs))
	if runs != 1 {
		t.Fatalf("uncommitted retry escaped: %d runs", runs)
	}
	_, err = s.db.Exec(`DROP TRIGGER reject_restart_audit`)
	requireStoreNoError(t, err)
	next, err := s.RecoverKVMPreflight(t.Context(), intent.RunID)
	requireStoreNoError(t, err)
	if next.ID == "" {
		t.Fatal("audit rollback spent retry budget")
	}
}

func TestKVMRestartPromotionKeepsExactPlanAndRechecksAuthority(t *testing.T) {
	for _, mode := range []string{"direct", "granted", "expired grant", "revoked actor"} {
		t.Run(mode, func(t *testing.T) {
			now := time.Now().UTC()
			s := testStore(t, &now)
			intent := kvmIntent(t, s)
			source, err := s.Run(t.Context(), intent.RunID)
			requireStoreNoError(t, err)
			_, err = s.FinishRun(t.Context(), source.ID, model.RunResult{Status: model.RunFailed})
			requireStoreNoError(t, err)
			credential, err := s.CreateTokenCredential(t.Context(), model.TokenCredentialSpec{Name: source.Actor, Digest: make([]byte, 32)})
			requireStoreNoError(t, err)
			_, err = s.RegisterUplink(t.Context(), "admin", model.UplinkSpec{Fingerprint: "SHA256:preflight-retry", Identity: source.Actor, TokenCredentialID: credential.ID, Admin: mode != "granted" && mode != "expired grant"})
			requireStoreNoError(t, err)
			_, err = s.ActivateTokenCredential(t.Context(), credential.ID)
			requireStoreNoError(t, err)
			spec := model.PromotionSpec{RepoID: source.RepoID, SourceBranch: "feature/source", SourceSHA: source.SHA, TargetRef: "main", PreviousSHA: strings.Repeat("b", 40), ResultSHA: source.SHA, Actor: source.Actor}
			if mode == "granted" || mode == "expired grant" {
				grant, err := s.AppendPromotionGrant(t.Context(), model.PromotionGrantSpec{RepoID: source.RepoID, SourceSHA: source.SHA, TargetRef: "main", Issuer: "admin", IssuerFingerprint: "SHA256:admin", ExpiresAt: now.Add(time.Hour)})
				requireStoreNoError(t, err)
				spec.GrantID = grant.ID
			}
			enqueued, original, err := s.EnqueuePromotionRun(t.Context(), model.RunSpec{RepoID: source.RepoID, RefKind: model.RefBranch, Ref: "promotion/main/test", SHA: source.SHA, TestedSHA: source.SHA, BaseSHA: spec.PreviousSHA, Trigger: "promotion", Actor: source.Actor}, spec)
			requireStoreNoError(t, err)
			run, err := s.ClaimNextRun(t.Context())
			requireStoreNoError(t, err)
			if run.ID != enqueued.ID {
				t.Fatal("wrong promotion claimed")
			}
			intent.RunID, intent.WorkflowName = run.ID, "trusted-"+run.ID
			reserveRestartPreflight(t, s, intent, true)
			if mode == "expired grant" {
				now = now.Add(2 * time.Hour)
			}
			if mode == "revoked actor" {
				requireStoreNoError(t, s.RevokeTokenCredential(t.Context(), credential.ID))
			}
			requireStoreNoError(t, s.RecoverOwnerState(t.Context()))
			next, err := s.RecoverKVMPreflight(t.Context(), run.ID)
			requireStoreNoError(t, err)
			after, err := s.Promotion(t.Context(), original.ID)
			requireStoreNoError(t, err)
			if after.Status != model.PromotionInterrupted || after.RunID != run.ID {
				t.Fatalf("original immutable promotion changed: %+v", after)
			}
			if mode == "expired grant" || mode == "revoked actor" {
				if next.ID != "" {
					t.Fatal("lost authority resurrected")
				}
				return
			}
			retry, err := s.PromotionByRun(t.Context(), next.ID)
			requireStoreNoError(t, err)
			if next.ID == "" || retry.ID == original.ID || retry.PreviousSHA != original.PreviousSHA || retry.ResultSHA != original.ResultSHA || retry.GrantID != original.GrantID || retry.SourceSHA != original.SourceSHA || retry.Status != model.PromotionPending {
				t.Fatalf("promotion plan/identity changed: %+v", retry)
			}
			lineage, err := s.PromotionLineage(t.Context(), retry.ID)
			requireStoreNoError(t, err)
			if lineage.RetryOf != original.ID {
				t.Fatalf("promotion retry not linked: %+v", lineage)
			}
			_, err = s.VerifyAuditState(t.Context())
			requireStoreNoError(t, err)
		})
	}
}

func TestKVMRestartDoesNotTurnKnownFailureIntoAutomaticRerun(t *testing.T) {
	now := time.Now().UTC()
	s := testStore(t, &now)
	intent := kvmIntent(t, s)
	reserveRestartPreflight(t, s, intent, false)
	requireStoreNoError(t, s.RecordKVMRefusal(t.Context(), intent.RunID, vmrunner.KVMRefusal{Code: "preflight_failed", ObservedAt: now}))
	requireStoreNoError(t, s.CompleteKVMPreflightCleanup(t.Context(), intent.RunID, "workflow-uid"))
	next, err := s.RecoverKVMPreflight(t.Context(), intent.RunID)
	requireStoreNoError(t, err)
	if next.ID != "" {
		t.Fatal("known suite failure automatically rerun")
	}
	state, err := s.KVMPreflight(t.Context(), intent.RunID)
	requireStoreNoError(t, err)
	if state.Refusal == nil || state.Refusal.Code != "preflight_failed" {
		t.Fatal("known negative evidence overwritten")
	}
}
