package store

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/vmrunner"
)

func vmIntent(t *testing.T, s *Store, repo model.Repository, branch string) vmrunner.ExecutionIntent {
	t.Helper()
	ctx := context.Background()
	enqueued, err := s.EnqueueRun(ctx, model.RunSpec{RepoID: repo.ID, RefKind: model.RefBranch, Ref: branch, SHA: strings.Repeat("a", 40), Actor: "tester", Trigger: "branch"})
	if err != nil {
		t.Fatal(err)
	}
	run, err := s.ClaimNextRun(ctx)
	if err != nil || run.ID != enqueued.ID {
		t.Fatalf("claim: %#v, %v", run, err)
	}
	upstream, err := s.Upstream(ctx, repo.UpstreamID)
	if err != nil {
		t.Fatal(err)
	}
	intent := vmrunner.ExecutionIntent{
		Spec:      vmrunner.VMRunSpec{RunID: run.ID, Repo: upstream.QualifiedRepo(repo.Name), CandidateSHA: run.TestedSHA, SuiteRevision: strings.Repeat("b", 40), GuestImageRef: "registry.example/guest@sha256:" + strings.Repeat("c", 64), Resources: vmrunner.VMResources{CPUCores: 1, MemoryMiB: 512}, Deadline: time.Minute},
		Namespace: "pipeline", Name: "vm-" + run.ID, ProfileIdentity: strings.Repeat("d", 64),
	}
	intent.Name = vmrunner.InstanceName(intent.Spec)
	return intent
}

func TestVMJournalAmbiguousCreateRetainsCapacity(t *testing.T) {
	ctx := context.Background()
	now := time.Now().UTC()
	s := testStore(t, &now)
	repo := createRepo(t, s)
	first := vmIntent(t, s, repo, "first")
	if _, err := s.ReserveVMExecution(ctx, first); err != nil {
		t.Fatal(err)
	}
	if err := s.SubmitVMExecution(ctx, first.Spec.RunID); err != nil {
		t.Fatal(err)
	}
	if err := s.BeginVMCleanup(ctx, first.Spec.RunID); err != nil {
		t.Fatal(err)
	}
	if err := s.CompleteVMCleanup(ctx, first.Spec.RunID, vmrunner.VMInstance{}); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("ambiguous request released reservation: %v", err)
	}
	second := vmIntent(t, s, repo, "second")
	if _, err := s.ReserveVMExecution(ctx, second); !errors.Is(err, ErrVMCapacity) {
		t.Fatalf("capacity reused after ambiguous create: %v", err)
	}
	// A delayed response can still bind while cancellation is pending, but
	// neither the UID nor the cleanup request itself releases the reservation.
	instance := vmrunner.VMInstance{Name: first.Name, UID: "late-vmi", SpecIdentity: vmrunner.SpecIdentity(first.Spec), CreatedAt: now}
	if err := s.BindVMExecution(ctx, first.Spec.RunID, instance); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ReserveVMExecution(ctx, second); !errors.Is(err, ErrVMCapacity) {
		t.Fatalf("binding released reservation: %v", err)
	}
	wrong := instance
	wrong.UID = "replacement"
	if err := s.CompleteVMCleanup(ctx, first.Spec.RunID, wrong); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("foreign cleanup discharged owned instance: %v", err)
	}
	if err := s.CompleteVMCleanup(ctx, first.Spec.RunID, instance); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ReserveVMExecution(ctx, second); err != nil {
		t.Fatalf("observed cleanup did not release reservation: %v", err)
	}
	if _, err := s.VerifyAuditChain(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestVMJournalAdmissionAndIdentityCannotChange(t *testing.T) {
	ctx := context.Background()
	for _, attack := range []string{"candidate", "repository", "credentialed", "release"} {
		t.Run(attack, func(t *testing.T) {
			now := time.Now().UTC()
			s := testStore(t, &now)
			intent := vmIntent(t, s, createRepo(t, s), "candidate")
			switch attack {
			case "candidate":
				intent.Spec.CandidateSHA = strings.Repeat("e", 40)
			case "repository":
				intent.Spec.Repo = "other/org/repo"
			case "credentialed":
				if _, err := s.db.Exec(`UPDATE runs SET credentialed=1 WHERE id=?`, intent.Spec.RunID); err != nil {
					t.Fatal(err)
				}
			case "release":
				if _, err := s.db.Exec(`UPDATE runs SET release=1 WHERE id=?`, intent.Spec.RunID); err != nil {
					t.Fatal(err)
				}
			}
			intent.Name = vmrunner.InstanceName(intent.Spec)
			if _, err := s.ReserveVMExecution(ctx, intent); !errors.Is(err, ErrInvalidState) {
				t.Fatalf("accepted %s: %v", attack, err)
			}
		})
	}
	now := time.Now().UTC()
	s := testStore(t, &now)
	intent := vmIntent(t, s, createRepo(t, s), "candidate")
	first, err := s.ReserveVMExecution(ctx, intent)
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"suite", "image", "profile", "name", "namespace"} {
		changed := intent
		switch field {
		case "suite":
			changed.Spec.SuiteRevision = strings.Repeat("e", 40)
		case "image":
			changed.Spec.GuestImageRef = "registry.example/other@sha256:" + strings.Repeat("f", 64)
		case "profile":
			changed.ProfileIdentity = strings.Repeat("e", 64)
		case "name":
			changed.Name = "other-name"
		case "namespace":
			changed.Namespace = "other-namespace"
		}
		if _, err := s.ReserveVMExecution(ctx, changed); !errors.Is(err, ErrInvalidState) && !errors.Is(err, ErrInvalid) {
			t.Fatalf("rebound %s: %v", field, err)
		}
	}
	retry, err := s.ReserveVMExecution(ctx, intent)
	if err != nil || !retry.CreatedAt.Equal(first.CreatedAt) || !retry.UpdatedAt.Equal(first.UpdatedAt) {
		t.Fatalf("retry changed intent: %#v, %v", retry, err)
	}
}

func TestVMJournalUnsubmittedCancellationAndAuditRollback(t *testing.T) {
	ctx := context.Background()
	now := time.Now().UTC()
	s := testStore(t, &now)
	intent := vmIntent(t, s, createRepo(t, s), "candidate")
	if _, err := s.ReserveVMExecution(ctx, intent); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`CREATE TRIGGER fail_vm_audit BEFORE INSERT ON audit_actions WHEN NEW.action='vm.create-submitted' BEGIN SELECT RAISE(ABORT,'audit unavailable'); END`); err != nil {
		t.Fatal(err)
	}
	if err := s.SubmitVMExecution(ctx, intent.Spec.RunID); err == nil {
		t.Fatal("audit failure accepted mutation")
	}
	value, err := s.VMExecution(ctx, intent.Spec.RunID)
	if err != nil || value.Submitted {
		t.Fatalf("submission survived audit rollback: %#v, %v", value, err)
	}
	if err := s.BeginVMCleanup(ctx, intent.Spec.RunID); err != nil {
		t.Fatal(err)
	}
	if err := s.SubmitVMExecution(ctx, intent.Spec.RunID); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("create submitted after cancellation: %v", err)
	}
	if err := s.CompleteVMCleanup(ctx, intent.Spec.RunID, vmrunner.VMInstance{}); err != nil {
		t.Fatal(err)
	}
	pending, err := s.PendingVMExecutions(ctx)
	if err != nil || len(pending) != 0 {
		t.Fatalf("canceled unsubmitted intent remains active: %#v, %v", pending, err)
	}
}
