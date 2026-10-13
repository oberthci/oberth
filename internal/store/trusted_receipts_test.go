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

func receiptRun(t *testing.T, s *Store, repo model.Repository) model.Run {
	t.Helper()
	ctx := context.Background()
	enqueued, err := s.EnqueueRun(ctx, model.RunSpec{
		RepoID: repo.ID, RefKind: model.RefBranch, Ref: "feature/receipt-test",
		SHA: strings.Repeat("a", 40), Actor: "tester@host", Trigger: "branch",
	})
	if err != nil {
		t.Fatal(err)
	}
	run, err := s.ClaimNextRun(ctx)
	if err != nil || run.ID != enqueued.ID {
		t.Fatalf("claim: %#v, %v", run, err)
	}
	return run
}

func validReceipt(runID string) vmrunner.TrustedTestReceipt {
	return vmrunner.TrustedTestReceipt{
		RunID:          runID,
		Profile:        vmrunner.KVMSuiteProfile,
		CandidateSHA:   strings.Repeat("a", 40),
		SuiteIdentity:  strings.Repeat("b", 40),
		GuestDigest:    "sha256:" + strings.Repeat("c", 64),
		ContractDigest: strings.Repeat("d", 64),
		Verdict:        vmrunner.VerdictPending,
		ExitCode:       -1,
		RestartCount:   -1,
	}
}

func TestWriteAndReadTrustedTestReceipt(t *testing.T) {
	ctx := context.Background()
	now := time.Now().UTC()
	s := testStore(t, &now)
	repo := createRepo(t, s)
	run := receiptRun(t, s, repo)

	receipt := validReceipt(run.ID)
	if err := s.WriteTrustedTestReceipt(ctx, receipt); err != nil {
		t.Fatal(err)
	}

	got, err := s.TrustedTestReceipt(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.RunID != run.ID || got.Profile != vmrunner.KVMSuiteProfile || got.Verdict != vmrunner.VerdictPending {
		t.Fatalf("receipt = %+v", got)
	}
}

func TestTrustedTestReceiptVerdictUpdate(t *testing.T) {
	ctx := context.Background()
	now := time.Now().UTC()
	s := testStore(t, &now)
	repo := createRepo(t, s)
	run := receiptRun(t, s, repo)

	// Write pending receipt.
	receipt := validReceipt(run.ID)
	if err := s.WriteTrustedTestReceipt(ctx, receipt); err != nil {
		t.Fatal(err)
	}

	// Update to passed verdict with kubelet data.
	receipt.Verdict = vmrunner.VerdictPassed
	receipt.PodUID = "pod-uid-123"
	receipt.ContainerID = "containerd://abc"
	receipt.ExitCode = 0
	receipt.RestartCount = 0
	receipt.TerminationHash = strings.Repeat("e", 64)
	if err := s.WriteTrustedTestReceipt(ctx, receipt); err != nil {
		t.Fatal(err)
	}

	got, err := s.TrustedTestReceipt(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Verdict != vmrunner.VerdictPassed || got.PodUID != "pod-uid-123" || got.ExitCode != 0 {
		t.Fatalf("updated receipt = %+v", got)
	}
}

func TestTrustedTestReceiptRefusesIdentityChange(t *testing.T) {
	ctx := context.Background()
	now := time.Now().UTC()
	s := testStore(t, &now)
	repo := createRepo(t, s)
	run := receiptRun(t, s, repo)

	receipt := validReceipt(run.ID)
	if err := s.WriteTrustedTestReceipt(ctx, receipt); err != nil {
		t.Fatal(err)
	}

	// Try to change an immutable field.
	receipt.CandidateSHA = strings.Repeat("f", 40)
	if err := s.WriteTrustedTestReceipt(ctx, receipt); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("identity change error = %v, want ErrInvalidState", err)
	}
}

func TestTrustedTestReceiptNotFound(t *testing.T) {
	ctx := context.Background()
	now := time.Now().UTC()
	s := testStore(t, &now)

	_, err := s.TrustedTestReceipt(ctx, "nonexistent-run")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("error = %v, want ErrNotFound", err)
	}
}

func TestTrustedTestReceiptAuditChain(t *testing.T) {
	ctx := context.Background()
	now := time.Now().UTC()
	s := testStore(t, &now)
	repo := createRepo(t, s)
	run := receiptRun(t, s, repo)

	receipt := validReceipt(run.ID)
	receipt.Verdict = vmrunner.VerdictPassed
	if err := s.WriteTrustedTestReceipt(ctx, receipt); err != nil {
		t.Fatal(err)
	}

	if _, err := s.VerifyAuditChain(ctx); err != nil {
		t.Fatalf("audit chain broken after receipt write: %v", err)
	}
}
