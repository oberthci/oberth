package store

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/vmrunner"
)

type journalBackend struct {
	beforeCreate func(vmrunner.VMRunSpec)
	instance     vmrunner.VMInstance
	createErr    error
	deleteErr    error
	deletes      int
	absent       bool
	now          time.Time
}

func (backend *journalBackend) CreateVMI(_ context.Context, _ string, spec vmrunner.VMRunSpec) (vmrunner.VMInstance, error) {
	if backend.beforeCreate != nil {
		backend.beforeCreate(spec)
	}
	backend.instance = vmrunner.VMInstance{Name: vmrunner.InstanceName(spec), UID: "owned-vmi", SpecIdentity: vmrunner.SpecIdentity(spec), CreatedAt: backend.now}
	if backend.createErr != nil {
		return vmrunner.VMInstance{Name: backend.instance.Name, SpecIdentity: backend.instance.SpecIdentity}, backend.createErr
	}
	return backend.instance, nil
}
func (backend *journalBackend) GetVMI(context.Context, string, string) (vmrunner.VMObservation, error) {
	if backend.absent {
		return vmrunner.VMObservation{}, errors.New("not found")
	}
	return vmrunner.VMObservation{Instance: backend.instance, Phase: "Succeeded"}, nil
}
func (backend *journalBackend) DeleteVMI(_ context.Context, _ string, instance vmrunner.VMInstance) error {
	backend.deletes++
	if instance != backend.instance {
		return errors.New("unowned instance")
	}
	if backend.deleteErr != nil {
		return backend.deleteErr
	}
	backend.absent = true
	return nil
}
func (*journalBackend) ListVMIs(context.Context, string, time.Duration) ([]vmrunner.VMInstance, error) {
	return nil, nil
}

func TestManagedVMJournalPrecedesCreateAndRequiresCleanup(t *testing.T) {
	ctx := context.Background()
	now := time.Now().UTC()
	s := testStore(t, &now)
	intent := vmIntent(t, s, createRepo(t, s), "candidate")
	backend := &journalBackend{now: now, deleteErr: errors.New("cleanup unavailable")}
	backend.beforeCreate = func(spec vmrunner.VMRunSpec) {
		stored, err := s.VMExecution(ctx, spec.RunID)
		if err != nil || !stored.Submitted {
			t.Fatalf("external create preceded durable submission: %#v, %v", stored, err)
		}
	}
	controller, err := vmrunner.NewController(vmrunner.Config{Namespace: "pipeline"}, backend)
	if err != nil {
		t.Fatal(err)
	}
	managed := &vmrunner.ManagedController{Controller: controller, Journal: s}
	result, err := managed.Execute(ctx, intent.Spec, intent.ProfileIdentity)
	if err == nil || result.CleanupComplete || result.ExitCode != vmrunner.ExitCodeUnknown {
		t.Fatalf("failed cleanup allowed success: %#v, %v", result, err)
	}
	pending, err := s.PendingVMExecutions(ctx)
	if err != nil || len(pending) != 1 || pending[0].Instance.UID != "owned-vmi" {
		t.Fatalf("cleanup obligation lost: %#v, %v", pending, err)
	}
	backend.deleteErr = nil
	// Recovery uses the durable identity and cannot fabricate a passing suite.
	if err := managed.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	pending, err = s.PendingVMExecutions(ctx)
	if err != nil || len(pending) != 0 {
		t.Fatalf("recovered cleanup remains pending: %#v, %v", pending, err)
	}
}

func TestManagedVMRecoversLateCreateButNeverAbsentUnknownIntent(t *testing.T) {
	for _, absent := range []bool{true, false} {
		t.Run(map[bool]string{true: "still-absent", false: "late-object"}[absent], func(t *testing.T) {
			ctx := context.Background()
			now := time.Now().UTC()
			s := testStore(t, &now)
			intent := vmIntent(t, s, createRepo(t, s), "candidate")
			backend := &journalBackend{now: now, createErr: context.DeadlineExceeded, absent: absent}
			controller, err := vmrunner.NewController(vmrunner.Config{Namespace: "pipeline"}, backend)
			if err != nil {
				t.Fatal(err)
			}
			managed := &vmrunner.ManagedController{Controller: controller, Journal: s}
			result, err := managed.Execute(ctx, intent.Spec, strings.Repeat("d", 64))
			if !errors.Is(err, context.DeadlineExceeded) || result.ExitCode != vmrunner.ExitCodeUnknown {
				t.Fatalf("ambiguous create became success: %#v, %v", result, err)
			}
			pending, readErr := s.PendingVMExecutions(ctx)
			if readErr != nil {
				t.Fatal(readErr)
			}
			if absent {
				if len(pending) != 1 || result.CleanupComplete || backend.deletes != 0 {
					t.Fatalf("absence released unknown create: %#v, %#v", result, pending)
				}
				backend.absent = false
				if err := managed.Recover(ctx); err != nil {
					t.Fatal(err)
				}
			} else if len(pending) != 0 || !result.CleanupComplete || backend.deletes != 1 {
				t.Fatalf("late owned object did not clean: %#v, %#v", result, pending)
			}
		})
	}
}

func TestVMJournalSurvivesOwnerRestart(t *testing.T) {
	ctx := context.Background()
	now := time.Now().UTC()
	path := t.TempDir() + "/restart.sqlite"
	s, err := Open(ctx, path, Options{Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Error(err)
		}
	})
	intent := vmIntent(t, s, createRepo(t, s), "candidate")
	if _, err := s.ReserveVMExecution(ctx, intent); err != nil {
		t.Fatal(err)
	}
	if err := s.SubmitVMExecution(ctx, intent.Spec.RunID); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(ctx, path, Options{Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	pending, err := s.PendingVMExecutions(ctx)
	if err != nil || len(pending) != 1 || !pending[0].Submitted || pending[0].Instance.UID != "" {
		t.Fatalf("restart lost unknown create: %#v, %v", pending, err)
	}
	backend := &journalBackend{now: now, instance: vmrunner.VMInstance{Name: intent.Name, UID: "recovered", SpecIdentity: vmrunner.SpecIdentity(intent.Spec), CreatedAt: now}}
	controller, err := vmrunner.NewController(vmrunner.Config{Namespace: "pipeline"}, backend)
	if err != nil {
		t.Fatal(err)
	}
	managed := &vmrunner.ManagedController{Controller: controller, Journal: s}
	if err := managed.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	value, err := s.VMExecution(ctx, intent.Spec.RunID)
	if err != nil || !value.Cleaned || value.Instance.UID != "recovered" || backend.deletes != 1 {
		t.Fatalf("recovery did not discharge owned resource: %#v, %v", value, err)
	}
}

func TestManagedVMDefinitiveRefusalReleasesCapacity(t *testing.T) {
	ctx := context.Background()
	now := time.Now().UTC()
	s := testStore(t, &now)
	intent := vmIntent(t, s, createRepo(t, s), "candidate")
	backend := &journalBackend{now: now, createErr: vmrunner.ErrCreateRefused, absent: true}
	controller, err := vmrunner.NewController(vmrunner.Config{Namespace: "pipeline"}, backend)
	if err != nil {
		t.Fatal(err)
	}
	managed := &vmrunner.ManagedController{Controller: controller, Journal: s}
	result, err := managed.Execute(ctx, intent.Spec, intent.ProfileIdentity)
	if !errors.Is(err, vmrunner.ErrCreateRefused) || !result.CleanupComplete || backend.deletes != 0 {
		t.Fatalf("definitive refusal did not finish without deleting: %#v, %v", result, err)
	}
	value, err := s.VMExecution(ctx, intent.Spec.RunID)
	if err != nil || !value.Rejected || !value.Cleaned {
		t.Fatalf("refusal receipt missing: %#v, %v", value, err)
	}
}

type credentialingVMJournal struct {
	*Store
	reserved vmrunner.Execution
}

func (journal *credentialingVMJournal) ReserveVMExecution(ctx context.Context, intent vmrunner.ExecutionIntent) (vmrunner.Execution, error) {
	value, err := journal.Store.ReserveVMExecution(ctx, intent)
	if err != nil {
		return value, err
	}
	journal.reserved = value
	// Deterministically interleave the scheduler's credential discovery after
	// reservation and before the managed controller attempts durable submission.
	return value, journal.SetRunCredentialed(ctx, intent.Spec.RunID)
}

func TestManagedVMRefusesCredentialedTransitionBeforeSubmission(t *testing.T) {
	ctx := context.Background()
	now := time.Now().UTC()
	s := testStore(t, &now)
	repo := createRepo(t, s)
	intent := vmIntent(t, s, repo, "candidate")
	creates := 0
	backend := &journalBackend{now: now, beforeCreate: func(vmrunner.VMRunSpec) { creates++ }}
	controller, err := vmrunner.NewController(vmrunner.Config{Namespace: "pipeline"}, backend)
	if err != nil {
		t.Fatal(err)
	}
	journal := &credentialingVMJournal{Store: s}
	managed := &vmrunner.ManagedController{Controller: controller, Journal: journal}
	result, err := managed.Execute(ctx, intent.Spec, intent.ProfileIdentity)
	if !errors.Is(err, ErrInvalidState) || creates != 0 || backend.deletes != 0 || result.ExitCode != vmrunner.ExitCodeUnknown {
		t.Fatalf("credentialed transition reached VM execution: result=%#v error=%v creates=%d deletes=%d", result, err, creates, backend.deletes)
	}
	pending, err := s.PendingVMExecutions(ctx)
	if err != nil || len(pending) != 1 || pending[0] != journal.reserved {
		t.Fatalf("refused submission changed or lost reserved ownership: %#v, %v", pending, err)
	}
	var submittedAudits int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM audit_actions WHERE action='vm.create-submitted' AND resource_id=?`, intent.Spec.RunID).Scan(&submittedAudits); err != nil {
		t.Fatal(err)
	}
	if submittedAudits != 0 {
		t.Fatalf("refused submission recorded %d submitted audits", submittedAudits)
	}
	other := vmIntent(t, s, repo, "other")
	if _, err := s.ReserveVMExecution(ctx, other); !errors.Is(err, ErrVMCapacity) {
		t.Fatalf("refusal silently released reserved capacity: %v", err)
	}
	// The ordinary recovery path can settle the retained, never-submitted
	// reservation without touching Kubernetes or undoing the credential flag.
	if err := managed.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	value, err := s.VMExecution(ctx, intent.Spec.RunID)
	if err != nil || !value.Cleaned || value.Submitted || value.Instance.UID != "" || creates != 0 || backend.deletes != 0 {
		t.Fatalf("unsubmitted recovery created/deleted a VM or retained capacity: %#v, %v", value, err)
	}
	run, err := s.Run(ctx, intent.Spec.RunID)
	if err != nil || !run.Credentialed {
		t.Fatalf("recovery changed credentialed admission: %#v, %v", run, err)
	}
	if _, err := s.ReserveVMExecution(ctx, other); err != nil {
		t.Fatalf("settled cleanup did not release capacity: %v", err)
	}
	if _, err := s.VerifyAuditChain(ctx); err != nil {
		t.Fatal(err)
	}
}

type submittedCredentialingVMJournal struct {
	*Store
	promotionErr error
}

func (journal *submittedCredentialingVMJournal) SubmitVMExecution(ctx context.Context, runID string) error {
	if err := journal.Store.SubmitVMExecution(ctx, runID); err != nil {
		return err
	}
	// Exercise the other ordering: submission wins, then credential discovery
	// attempts to promote the same run before the external create starts.
	journal.promotionErr = journal.SetRunCredentialed(ctx, runID)
	return nil
}

func TestManagedVMPreventsCredentialPromotionUntilCleanup(t *testing.T) {
	ctx := context.Background()
	now := time.Now().UTC()
	s := testStore(t, &now)
	intent := vmIntent(t, s, createRepo(t, s), "candidate")
	cleanupErr := errors.New("cleanup unavailable")
	backend := &journalBackend{now: now, deleteErr: cleanupErr}
	createdCredentialed := false
	backend.beforeCreate = func(spec vmrunner.VMRunSpec) {
		run, err := s.Run(ctx, spec.RunID)
		if err != nil {
			t.Fatal(err)
		}
		createdCredentialed = run.Credentialed
	}
	controller, err := vmrunner.NewController(vmrunner.Config{Namespace: "pipeline"}, backend)
	if err != nil {
		t.Fatal(err)
	}
	journal := &submittedCredentialingVMJournal{Store: s}
	managed := &vmrunner.ManagedController{Controller: controller, Journal: journal}
	result, err := managed.Execute(ctx, intent.Spec, intent.ProfileIdentity)
	if !errors.Is(journal.promotionErr, ErrInvalidState) || createdCredentialed || !errors.Is(err, cleanupErr) || result.CleanupComplete {
		t.Fatalf("credential promotion crossed live VM ownership: promotion=%v createdCredentialed=%v result=%#v error=%v", journal.promotionErr, createdCredentialed, result, err)
	}
	value, err := s.VMExecution(ctx, intent.Spec.RunID)
	if err != nil || !value.Submitted || value.Instance.UID == "" || value.Cleaned {
		t.Fatalf("failed cleanup lost durable ownership: %#v, %v", value, err)
	}
	if err := s.SetRunCredentialed(ctx, intent.Spec.RunID); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("credential promotion ignored pending cleanup: %v", err)
	}
	run, err := s.Run(ctx, intent.Spec.RunID)
	if err != nil || run.Credentialed {
		t.Fatalf("refused promotion changed the run: %#v, %v", run, err)
	}
	backend.deleteErr = nil
	if err := managed.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	if err := s.SetRunCredentialed(ctx, intent.Spec.RunID); err != nil {
		t.Fatalf("observed cleanup did not end the credential conflict: %v", err)
	}
	run, err = s.Run(ctx, intent.Spec.RunID)
	if err != nil || !run.Credentialed {
		t.Fatalf("ordinary credential transition failed after cleanup: %#v, %v", run, err)
	}
}
