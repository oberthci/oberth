package store

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/vmrunner"
)

func kvmIntent(t *testing.T, s *Store) vmrunner.KVMIntent {
	t.Helper()
	repo := createRepo(t, s)
	legacy := vmIntent(t, s, repo, "kvm")
	now := s.now()
	return vmrunner.KVMIntent{
		RunID: legacy.Spec.RunID, RepoID: repo.ID, Repository: legacy.Spec.Repo, CandidateSHA: legacy.Spec.CandidateSHA, BaselineSHA: strings.Repeat("b", 40),
		Contract:  vmrunner.Contract{Version: 2, Profile: vmrunner.KVMSuiteAMD64Profile, SuiteSHA256: strings.Repeat("c", 64), GuestSHA256: strings.Repeat("d", 64), Artifacts: []string{"build/program"}},
		Recipe:    vmrunner.KVMRecipe{SuiteSHA256: strings.Repeat("c", 64), GuestSHA256: strings.Repeat("d", 64), HarnessImage: "registry.example/harness@sha256:" + strings.Repeat("e", 64), SuiteURL: "https://example.org/suite", GuestURL: "https://example.org/guest"},
		Namespace: "pipelines", WorkflowName: "trusted-" + legacy.Spec.RunID, WorkflowSpecDigest: strings.Repeat("f", 64), InventoryDigest: strings.Repeat("a", 64), CaseCount: 2,
		AdmittedAt: now, Deadline: now.Add(time.Minute),
	}
}
func kvmReceipt(intent vmrunner.KVMIntent) vmrunner.KVMReceipt {
	digest := vmrunner.KVMIntentDigest(intent)
	return vmrunner.KVMReceipt{
		IntentDigest: digest, WorkflowUID: "workflow-uid",
		Binding:     vmrunner.KVMTerminationBinding{Namespace: intent.Namespace, WorkflowName: intent.WorkflowName, WorkflowUID: "workflow-uid", PodName: "trusted-pod", PodUID: "pod-uid", ContainerID: "containerd://container", HarnessImageRef: intent.Recipe.HarnessImage, AdmittedAt: intent.AdmittedAt, Deadline: intent.Deadline},
		Report:      vmrunner.KVMReport{Version: 1, IntentDigest: digest, InventoryDigest: intent.InventoryDigest, ArtifactSetDigest: strings.Repeat("b", 64), CaseCount: intent.CaseCount},
		Termination: vmrunner.KVMTerminationRecord{ImageID: "sha256:" + strings.Repeat("e", 64), ContainerID: "containerd://container", PodUID: "pod-uid", WorkflowUID: "workflow-uid", StartedAt: intent.AdmittedAt, FinishedAt: intent.AdmittedAt},
		FinishedAt:  intent.AdmittedAt,
	}
}
func requireKVMStateError(t *testing.T, err error) {
	t.Helper()
	if !errors.Is(err, ErrInvalidState) {
		t.Fatalf("wanted invalid state, got %v", err)
	}
}
func TestKVMPreflightReleaseInterlocksAndReceipt(t *testing.T) {
	ctx := context.Background()
	now := time.Now().UTC()
	s := testStore(t, &now)
	intent := kvmIntent(t, s)
	// Release admission can set credentialed before the credential-free preflight.
	_, err := s.db.Exec(`UPDATE runs SET release=1,credentialed=1 WHERE id=?`, intent.RunID)
	mustPilot(t, err)
	_, err = s.ReserveKVMPreflight(ctx, intent)
	mustPilot(t, err)
	_, err = s.SetRunJobName(ctx, intent.RunID, "main-workflow")
	requireKVMStateError(t, err)
	mustPilot(t, s.SubmitKVMPreflight(ctx, intent.RunID))
	mustPilot(t, s.BindKVMPreflight(ctx, intent.RunID, "workflow-uid"))
	mustPilot(t, s.RecordKVMReceipt(ctx, intent.RunID, kvmReceipt(intent)))
	value, err := s.KVMPreflight(ctx, intent.RunID)
	mustPilot(t, err)
	if value.Receipt == nil || value.Cleaned {
		t.Fatalf("receipt must persist before cleanup: %#v", value)
	}
	_, err = s.SetRunJobName(ctx, intent.RunID, "main-workflow")
	requireKVMStateError(t, err)
	requireKVMStateError(t, s.CompleteKVMPreflightCleanup(ctx, intent.RunID, "replacement"))
	mustPilot(t, s.CompleteKVMPreflightCleanup(ctx, intent.RunID, "workflow-uid"))
	mustPilot(t, s.CompleteKVMPreflightCleanup(ctx, intent.RunID, "workflow-uid"))
	_, err = s.SetRunJobName(ctx, intent.RunID, "main-workflow")
	mustPilot(t, err)
	value, err = s.KVMPreflight(ctx, intent.RunID)
	mustPilot(t, err)
	if !value.Cleaned || value.Receipt == nil {
		t.Fatal("cleanup lost receipt")
	}
	pending, err := s.PendingKVMPreflights(ctx)
	mustPilot(t, err)
	if len(pending) != 0 {
		t.Fatal("cleaned preflight pending")
	}
	_, err = s.VerifyAuditChain(ctx)
	mustPilot(t, err)
}
func TestKVMPreflightAdmissionAndIntentImmutability(t *testing.T) {
	for _, attack := range []string{"candidate", "repo-id", "repository", "inactive", "main-job", "future", "expired"} {
		t.Run(attack, func(t *testing.T) {
			ctx := context.Background()
			now := time.Now().UTC()
			s := testStore(t, &now)
			intent := kvmIntent(t, s)
			switch attack {
			case "candidate":
				intent.CandidateSHA = strings.Repeat("f", 40)
			case "repo-id":
				intent.RepoID++
			case "repository":
				intent.Repository = "other/org/repo"
			case "inactive":
				_, err := s.db.Exec(`UPDATE runs SET status='failed' WHERE id=?`, intent.RunID)
				mustPilot(t, err)
			case "main-job":
				_, err := s.SetRunJobName(ctx, intent.RunID, "main-job")
				mustPilot(t, err)
			case "future":
				intent.AdmittedAt = now.Add(time.Second)
			case "expired":
				intent.AdmittedAt = now.Add(-2 * time.Minute)
				intent.Deadline = now.Add(-time.Minute)
			}
			_, err := s.ReserveKVMPreflight(ctx, intent)
			requireKVMStateError(t, err)
		})
	}
	ctx := context.Background()
	now := time.Now().UTC()
	s := testStore(t, &now)
	intent := kvmIntent(t, s)
	_, err := s.ReserveKVMPreflight(ctx, intent)
	mustPilot(t, err)
	_, err = s.ReserveKVMPreflight(ctx, intent)
	mustPilot(t, err)
	for name, change := range map[string]func(*vmrunner.KVMIntent){
		"suite": func(i *vmrunner.KVMIntent) {
			i.Contract.SuiteSHA256 = strings.Repeat("f", 64)
			i.Recipe.SuiteSHA256 = i.Contract.SuiteSHA256
		},
		"baseline":  func(i *vmrunner.KVMIntent) { i.BaselineSHA = strings.Repeat("f", 40) },
		"workflow":  func(i *vmrunner.KVMIntent) { i.WorkflowName = "different" },
		"inventory": func(i *vmrunner.KVMIntent) { i.InventoryDigest = strings.Repeat("f", 64) },
		"harness": func(i *vmrunner.KVMIntent) {
			i.Recipe.HarnessImage = "registry.example/harness@sha256:" + strings.Repeat("f", 64)
		},
	} {
		t.Run(name, func(t *testing.T) {
			changed := intent
			change(&changed)
			_, err := s.ReserveKVMPreflight(ctx, changed)
			requireKVMStateError(t, err)
		})
	}
	requireKVMStateError(t, s.SetRunCredentialed(ctx, intent.RunID))
	mustPilot(t, s.CompleteKVMPreflightCleanup(ctx, intent.RunID, ""))
	_, err = s.SetRunJobName(ctx, intent.RunID, "main-job")
	requireKVMStateError(t, err)
	mustPilot(t, s.SetRunCredentialed(ctx, intent.RunID))
	requireKVMStateError(t, s.SubmitKVMPreflight(ctx, intent.RunID))
}
func TestKVMPreflightUncertainCreateAndKindIsolation(t *testing.T) {
	ctx := context.Background()
	now := time.Now().UTC()
	s := testStore(t, &now)
	intent := kvmIntent(t, s)
	_, err := s.ReserveKVMPreflight(ctx, intent)
	mustPilot(t, err)
	if _, err = s.VMExecution(ctx, intent.RunID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("legacy accessor aliased KVM: %v", err)
	}
	pending, err := s.PendingVMExecutions(ctx)
	mustPilot(t, err)
	if len(pending) != 0 {
		t.Fatal("legacy pending scan included KVM")
	}
	if err = s.BeginVMCleanup(ctx, intent.RunID); err == nil {
		t.Fatal("legacy method changed KVM")
	}
	kp, err := s.PendingKVMPreflights(ctx)
	mustPilot(t, err)
	if len(kp) != 1 {
		t.Fatal("missing KVM reservation")
	}
	mustPilot(t, s.SubmitKVMPreflight(ctx, intent.RunID))
	requireKVMStateError(t, s.CompleteKVMPreflightCleanup(ctx, intent.RunID, ""))
	requireKVMStateError(t, s.SetRunCredentialed(ctx, intent.RunID))
	repo, err := s.Repository(ctx, intent.RepoID)
	mustPilot(t, err)
	legacy := vmIntent(t, s, repo, "other")
	_, err = s.ReserveVMExecution(ctx, legacy)
	if !errors.Is(err, ErrVMCapacity) {
		t.Fatalf("ambiguous KVM freed capacity: %v", err)
	}
	// Late bind remains possible for cleanup after cancellation.
	_, err = s.db.Exec(`UPDATE runs SET status='failed' WHERE id=?`, intent.RunID)
	mustPilot(t, err)
	mustPilot(t, s.BindKVMPreflight(ctx, intent.RunID, "workflow-uid"))
	requireKVMStateError(t, s.BindKVMPreflight(ctx, intent.RunID, "replacement"))
	requireKVMStateError(t, s.RejectKVMPreflight(ctx, intent.RunID))
	requireKVMStateError(t, s.RecordKVMReceipt(ctx, intent.RunID, kvmReceipt(intent)))
	mustPilot(t, s.CompleteKVMPreflightCleanup(ctx, intent.RunID, "workflow-uid"))
	_, err = s.ReserveVMExecution(ctx, legacy)
	mustPilot(t, err)
	if _, err = s.KVMPreflight(ctx, legacy.Spec.RunID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("KVM aliased legacy: %v", err)
	}
}
func TestKVMPreflightLifecycleAndReceiptAttacks(t *testing.T) {
	ctx := context.Background()
	now := time.Now().UTC()
	s := testStore(t, &now)
	intent := kvmIntent(t, s)
	_, err := s.ReserveKVMPreflight(ctx, intent)
	mustPilot(t, err)
	receipt := kvmReceipt(intent)
	requireKVMStateError(t, s.BindKVMPreflight(ctx, intent.RunID, "workflow-uid"))
	requireKVMStateError(t, s.RejectKVMPreflight(ctx, intent.RunID))
	requireKVMStateError(t, s.RecordKVMReceipt(ctx, intent.RunID, receipt))
	mustPilot(t, s.SubmitKVMPreflight(ctx, intent.RunID))
	requireKVMStateError(t, s.SubmitKVMPreflight(ctx, intent.RunID))
	requireKVMStateError(t, s.RecordKVMReceipt(ctx, intent.RunID, receipt))
	mustPilot(t, s.BindKVMPreflight(ctx, intent.RunID, "workflow-uid"))
	mustPilot(t, s.BindKVMPreflight(ctx, intent.RunID, "workflow-uid"))
	for name, change := range map[string]func(*vmrunner.KVMReceipt){
		"foreign-workflow": func(r *vmrunner.KVMReceipt) {
			r.WorkflowUID = "other"
			r.Binding.WorkflowUID = "other"
			r.Termination.WorkflowUID = "other"
		},
		"replay": func(r *vmrunner.KVMReceipt) {
			r.IntentDigest = strings.Repeat("f", 64)
			r.Report.IntentDigest = r.IntentDigest
		},
		"inventory":     func(r *vmrunner.KVMReceipt) { r.Report.InventoryDigest = strings.Repeat("f", 64) },
		"missing-cases": func(r *vmrunner.KVMReceipt) { r.Report.CaseCount-- },
		"exit":          func(r *vmrunner.KVMReceipt) { r.Termination.ExitCode = 1 },
		"restart":       func(r *vmrunner.KVMReceipt) { r.Termination.RestartCount = 1 },
		"image":         func(r *vmrunner.KVMReceipt) { r.Termination.ImageID = "sha256:" + strings.Repeat("f", 64) },
		"pod":           func(r *vmrunner.KVMReceipt) { r.Termination.PodUID = "different" },
		"deadline": func(r *vmrunner.KVMReceipt) {
			r.FinishedAt = intent.Deadline.Add(time.Second)
			r.Termination.FinishedAt = r.FinishedAt
		},
		"future": func(r *vmrunner.KVMReceipt) {
			r.FinishedAt = now.Add(time.Second)
			r.Termination.FinishedAt = r.FinishedAt
		},
	} {
		t.Run(name, func(t *testing.T) {
			changed := receipt
			change(&changed)
			requireKVMStateError(t, s.RecordKVMReceipt(ctx, intent.RunID, changed))
		})
	}
	mustPilot(t, s.RecordKVMReceipt(ctx, intent.RunID, receipt))
	mustPilot(t, s.RecordKVMReceipt(ctx, intent.RunID, receipt))
	receipt.Report.ArtifactSetDigest = strings.Repeat("f", 64)
	requireKVMStateError(t, s.RecordKVMReceipt(ctx, intent.RunID, receipt))
	// SQL writes cannot replace accepted evidence; reads also reject corrupt evidence.
	receipt.Termination.ExitCode = 1
	body, err := json.Marshal(receipt)
	mustPilot(t, err)
	_, err = s.db.Exec(`UPDATE vm_executions SET receipt_json=? WHERE run_id=?`, string(body), intent.RunID)
	requireVMJournalGuard(t, err, "VM execution evidence is immutable")
	// Exercise decoding independently without removing the database guard.
	_, err = scanKVMExecution(s.db.QueryRow(`SELECT execution_kind,intent_json,spec_identity,
        workflow_uid,submitted,rejected,cleaned,?,refusal_json FROM vm_executions WHERE run_id=?`, string(body), intent.RunID))
	requireKVMStateError(t, err)
}
func TestKVMPreflightDefinitiveRejectionAndAuditRollback(t *testing.T) {
	ctx := context.Background()
	now := time.Now().UTC()
	s := testStore(t, &now)
	intent := kvmIntent(t, s)
	_, err := s.ReserveKVMPreflight(ctx, intent)
	mustPilot(t, err)
	_, err = s.db.Exec(`CREATE TRIGGER fail_kvm_audit BEFORE INSERT ON audit_actions WHEN NEW.action='kvm.create-submitted' BEGIN SELECT RAISE(ABORT,'audit unavailable'); END`)
	mustPilot(t, err)
	if err = s.SubmitKVMPreflight(ctx, intent.RunID); err == nil {
		t.Fatal("audit failure accepted")
	}
	value, err := s.KVMPreflight(ctx, intent.RunID)
	mustPilot(t, err)
	if value.Submitted {
		t.Fatal("submission survived audit rollback")
	}
	_, err = s.db.Exec(`DROP TRIGGER fail_kvm_audit`)
	mustPilot(t, err)
	mustPilot(t, s.SubmitKVMPreflight(ctx, intent.RunID))
	mustPilot(t, s.RejectKVMPreflight(ctx, intent.RunID))
	mustPilot(t, s.RejectKVMPreflight(ctx, intent.RunID))
	requireKVMStateError(t, s.BindKVMPreflight(ctx, intent.RunID, "late"))
	requireKVMStateError(t, s.RecordKVMReceipt(ctx, intent.RunID, kvmReceipt(intent)))
	mustPilot(t, s.CompleteKVMPreflightCleanup(ctx, intent.RunID, ""))
}
func TestKVMPreflightSubmissionRechecksParent(t *testing.T) {
	for _, column := range []string{"job_name", "tested_sha", "status"} {
		t.Run(column, func(t *testing.T) {
			ctx := context.Background()
			now := time.Now().UTC()
			s := testStore(t, &now)
			intent := kvmIntent(t, s)
			_, err := s.ReserveKVMPreflight(ctx, intent)
			mustPilot(t, err)
			value := "different"
			if column == "status" {
				value = "failed"
			}
			_, err = s.db.Exec(`UPDATE runs SET `+column+`=? WHERE id=?`, value, intent.RunID)
			mustPilot(t, err)
			requireKVMStateError(t, s.SubmitKVMPreflight(ctx, intent.RunID))
			mustPilot(t, s.CompleteKVMPreflightCleanup(ctx, intent.RunID, ""))
		})
	}
}
func TestKVMJournalMigration20PreservesLegacyExecution(t *testing.T) {
	ctx := context.Background()
	now := time.Now().UTC()
	path := filepath.Join(t.TempDir(), "migration.db")
	s, err := Open(ctx, path, Options{Now: func() time.Time { return now }})
	mustPilot(t, err)
	legacy := vmIntent(t, s, createRepo(t, s), "legacy")
	_, err = s.ReserveVMExecution(ctx, legacy)
	mustPilot(t, err)
	mustPilot(t, s.SubmitVMExecution(ctx, legacy.Spec.RunID))
	// Recreate v19's exact legacy journal and retain its ambiguous reservation.
	_, err = s.db.Exec(`ALTER TABLE vm_executions RENAME TO saved_executions; DROP INDEX vm_execution_capacity;` + migrations[12].sql + `
 INSERT INTO vm_executions(run_id,intent_json,spec_identity,instance_json,submitted,rejected,cleaning,cleaned,created_at,updated_at)
 SELECT run_id,intent_json,spec_identity,instance_json,submitted,rejected,cleaning,cleaned,created_at,updated_at FROM saved_executions;
 DROP TABLE saved_executions; ALTER TABLE schedule_fires DROP COLUMN last_fired_at; DELETE FROM schema_migrations WHERE version=32; DROP TRIGGER run_schedule_entry_immutable; ALTER TABLE runs DROP COLUMN schedule_entry; DELETE FROM schema_migrations WHERE version=31; DROP TRIGGER run_timing_claim; DROP TRIGGER run_timing_grant; DROP TRIGGER run_timing_park; DROP TRIGGER run_timing_terminal; DROP TABLE step_timings; DROP TABLE run_admission_timings; DROP TABLE run_timings; DROP INDEX runs_admission_idx; ALTER TABLE runs DROP COLUMN admission_sequence; DROP TABLE kubevirt_grants; DROP TABLE uplink_scopes; ALTER TABLE issues DROP COLUMN scope_upstream_id; ALTER TABLE step_results DROP COLUMN termination_json; DELETE FROM schema_migrations WHERE version>=20;`)
	mustPilot(t, err)
	mustPilot(t, s.Close())
	s, err = Open(ctx, path, Options{Now: func() time.Time { return now }})
	mustPilot(t, err)
	defer func() { mustPilot(t, s.Close()) }()
	value, err := s.VMExecution(ctx, legacy.Spec.RunID)
	mustPilot(t, err)
	if !value.Submitted || value.Cleaned || value.Intent.Spec.RunID != legacy.Spec.RunID {
		t.Fatalf("migration lost legacy state: %#v", value)
	}
	var slot string
	mustPilot(t, s.db.QueryRow(`SELECT run_id FROM vm_capacity_slots WHERE slot=1`).Scan(&slot))
	if slot != legacy.Spec.RunID {
		t.Fatal("migration lost capacity")
	}
	pending, err := s.PendingKVMPreflights(ctx)
	mustPilot(t, err)
	if len(pending) != 0 {
		t.Fatal("legacy migrated as KVM")
	}
	mustPilot(t, s.BeginVMCleanup(ctx, legacy.Spec.RunID))
	requireKVMStateError(t, s.CompleteVMCleanup(ctx, legacy.Spec.RunID, vmrunner.VMInstance{}))
}

func TestKVMPreflightReceiptAndCleanupAuditFailuresRetainObligation(t *testing.T) {
	ctx := context.Background()
	now := time.Now().UTC()
	s := testStore(t, &now)
	intent := kvmIntent(t, s)
	_, err := s.ReserveKVMPreflight(ctx, intent)
	mustPilot(t, err)
	mustPilot(t, s.SubmitKVMPreflight(ctx, intent.RunID))
	mustPilot(t, s.BindKVMPreflight(ctx, intent.RunID, "workflow-uid"))
	for _, action := range []string{"kvm.receipt", "kvm.cleanup-observed"} {
		_, err = s.db.Exec(`CREATE TRIGGER fail_kvm_audit BEFORE INSERT ON audit_actions WHEN NEW.action='` + action + `' BEGIN SELECT RAISE(ABORT,'audit unavailable'); END`)
		mustPilot(t, err)
		if action == "kvm.receipt" {
			err = s.RecordKVMReceipt(ctx, intent.RunID, kvmReceipt(intent))
		} else {
			err = s.CompleteKVMPreflightCleanup(ctx, intent.RunID, "workflow-uid")
		}
		if err == nil {
			t.Fatalf("%s accepted despite audit failure", action)
		}
		value, err := s.KVMPreflight(ctx, intent.RunID)
		mustPilot(t, err)
		if value.Cleaned || (action == "kvm.receipt" && value.Receipt != nil) {
			t.Fatalf("%s survived failed audit", action)
		}
		var occupied int
		mustPilot(t, s.db.QueryRow(`SELECT COUNT(*) FROM vm_capacity_slots WHERE run_id=?`, intent.RunID).Scan(&occupied))
		if occupied != 1 {
			t.Fatal("failed cleanup released capacity")
		}
		_, err = s.db.Exec(`DROP TRIGGER fail_kvm_audit`)
		mustPilot(t, err)
		if action == "kvm.receipt" {
			mustPilot(t, s.RecordKVMReceipt(ctx, intent.RunID, kvmReceipt(intent)))
		}
	}
	mustPilot(t, s.CompleteKVMPreflightCleanup(ctx, intent.RunID, "workflow-uid"))
}

func TestKVMPreflightSharesCapacityWithLegacyReservation(t *testing.T) {
	ctx := context.Background()
	now := time.Now().UTC()
	s := testStore(t, &now)
	intent := kvmIntent(t, s)
	repo, err := s.Repository(ctx, intent.RepoID)
	mustPilot(t, err)
	legacy := vmIntent(t, s, repo, "legacy")
	_, err = s.ReserveVMExecution(ctx, legacy)
	mustPilot(t, err)
	_, err = s.ReserveKVMPreflight(ctx, intent)
	if !errors.Is(err, ErrVMCapacity) {
		t.Fatalf("KVM ignored occupied legacy slot: %v", err)
	}
	mustPilot(t, s.BeginVMCleanup(ctx, legacy.Spec.RunID))
	mustPilot(t, s.CompleteVMCleanup(ctx, legacy.Spec.RunID, vmrunner.VMInstance{}))
	_, err = s.ReserveKVMPreflight(ctx, intent)
	mustPilot(t, err)
}

func TestKVMPreflightRecoveryRetainsCleanedUndeliveredParent(t *testing.T) {
	ctx := context.Background()
	now := time.Now().UTC()
	s := testStore(t, &now)
	intent := kvmIntent(t, s)
	_, err := s.ReserveKVMPreflight(ctx, intent)
	mustPilot(t, err)
	mustPilot(t, s.CompleteKVMPreflightCleanup(ctx, intent.RunID, ""))
	pending, err := s.PendingKVMPreflights(ctx)
	mustPilot(t, err)
	if len(pending) != 1 || !pending[0].Cleaned || pending[0].Intent.RunID != intent.RunID {
		t.Fatalf("lost cleaned undelivered parent: %#v", pending)
	}
	_, err = s.db.Exec(`UPDATE runs SET status='interrupted' WHERE id=?`, intent.RunID)
	mustPilot(t, err)
	pending, err = s.PendingKVMPreflights(ctx)
	mustPilot(t, err)
	if len(pending) != 0 {
		t.Fatal("terminal cleaned parent still pending")
	}
}

func TestKVMPreflightRefusalIsImmutableAndSurvivesCleanup(t *testing.T) {
	ctx := context.Background()
	now := time.Now().UTC()
	s := testStore(t, &now)
	intent := kvmIntent(t, s)
	_, err := s.ReserveKVMPreflight(ctx, intent)
	mustPilot(t, err)
	mustPilot(t, s.SubmitKVMPreflight(ctx, intent.RunID))
	mustPilot(t, s.BindKVMPreflight(ctx, intent.RunID, "workflow-uid"))
	terminated := kvmReceipt(intent).Termination
	terminated.RestartCount = 1
	refusal := vmrunner.KVMRefusal{Code: "observation_refused", ObservedAt: now, Termination: &terminated}
	mustPilot(t, s.RecordKVMRefusal(ctx, intent.RunID, refusal))
	mustPilot(t, s.RecordKVMRefusal(ctx, intent.RunID, refusal))
	replacement := refusal
	replacement.Code = "canceled"
	requireKVMStateError(t, s.RecordKVMRefusal(ctx, intent.RunID, replacement))
	requireKVMStateError(t, s.RecordKVMReceipt(ctx, intent.RunID, kvmReceipt(intent)))
	mustPilot(t, s.CompleteKVMPreflightCleanup(ctx, intent.RunID, "workflow-uid"))
	value, err := s.KVMPreflight(ctx, intent.RunID)
	mustPilot(t, err)
	if !value.Cleaned || value.Receipt != nil || value.Refusal == nil || value.Refusal.Termination.RestartCount != 1 {
		t.Fatalf("negative evidence lost: %#v", value)
	}
	_, err = s.SetRunJobName(ctx, intent.RunID, "main")
	requireKVMStateError(t, err)
}
func TestKVMPreflightRefusalRejectsReplayAndCannotReplaceSuccess(t *testing.T) {
	ctx := context.Background()
	now := time.Now().UTC()
	s := testStore(t, &now)
	intent := kvmIntent(t, s)
	_, err := s.ReserveKVMPreflight(ctx, intent)
	mustPilot(t, err)
	for _, refusal := range []vmrunner.KVMRefusal{
		{Code: "free-form log", ObservedAt: now}, {Code: "canceled"}, {Code: "deadline", ObservedAt: now.Add(time.Second)},
		{Code: "observation_refused", ObservedAt: now, Termination: &vmrunner.KVMTerminationRecord{WorkflowUID: "foreign"}},
	} {
		requireKVMStateError(t, s.RecordKVMRefusal(ctx, intent.RunID, refusal))
	}
	mustPilot(t, s.SubmitKVMPreflight(ctx, intent.RunID))
	mustPilot(t, s.BindKVMPreflight(ctx, intent.RunID, "workflow-uid"))
	mustPilot(t, s.RecordKVMReceipt(ctx, intent.RunID, kvmReceipt(intent)))
	requireKVMStateError(t, s.RecordKVMRefusal(ctx, intent.RunID, vmrunner.KVMRefusal{Code: "canceled", ObservedAt: now}))
}
