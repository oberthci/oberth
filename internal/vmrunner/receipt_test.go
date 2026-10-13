package vmrunner

import (
	"strings"
	"testing"
	"time"
)

func TestEvaluateKubeletVerdictPassingCase(t *testing.T) {
	t.Parallel()
	start := time.Now()
	record := KubeletRecord{
		PodUID:       "uid-123",
		ContainerID:  "containerd://abc",
		ExitCode:     0,
		RestartCount: 0,
		Message:      `{"cases":3,"passed":3}`,
		FinishedAt:   start.Add(5 * time.Minute),
	}
	verdict, reason := EvaluateKubeletVerdict(record, "sha256:expected", 30*time.Minute, start)
	if verdict != VerdictPassed || reason != "" {
		t.Fatalf("verdict = %s, reason = %q", verdict, reason)
	}
}

func TestEvaluateKubeletVerdictExitNonzero(t *testing.T) {
	t.Parallel()
	start := time.Now()
	record := KubeletRecord{
		PodUID:       "uid-123",
		ContainerID:  "containerd://abc",
		ExitCode:     1,
		RestartCount: 0,
		FinishedAt:   start.Add(5 * time.Minute),
	}
	verdict, reason := EvaluateKubeletVerdict(record, "sha256:expected", 30*time.Minute, start)
	if verdict != VerdictRefused {
		t.Fatalf("verdict = %s, want refused", verdict)
	}
	if !strings.Contains(reason, "exit_nonzero") {
		t.Fatalf("reason = %q, want exit_nonzero", reason)
	}
}

func TestEvaluateKubeletVerdictRestartDetected(t *testing.T) {
	t.Parallel()
	start := time.Now()
	record := KubeletRecord{
		PodUID:       "uid-123",
		ContainerID:  "containerd://abc",
		ExitCode:     0,
		RestartCount: 1,
		FinishedAt:   start.Add(5 * time.Minute),
	}
	verdict, reason := EvaluateKubeletVerdict(record, "sha256:expected", 30*time.Minute, start)
	if verdict != VerdictRefused {
		t.Fatalf("verdict = %s, want refused", verdict)
	}
	if !strings.Contains(reason, "restart_detected") {
		t.Fatalf("reason = %q, want restart_detected", reason)
	}
}

func TestEvaluateKubeletVerdictTimeout(t *testing.T) {
	t.Parallel()
	start := time.Now()
	record := KubeletRecord{
		PodUID:       "uid-123",
		ContainerID:  "containerd://abc",
		ExitCode:     0,
		RestartCount: 0,
		FinishedAt:   start.Add(45 * time.Minute),
	}
	verdict, reason := EvaluateKubeletVerdict(record, "sha256:expected", 30*time.Minute, start)
	if verdict != VerdictRefused {
		t.Fatalf("verdict = %s, want refused", verdict)
	}
	if !strings.Contains(reason, "timeout") {
		t.Fatalf("reason = %q, want timeout", reason)
	}
}

func TestValidateReceiptRefusesIncomplete(t *testing.T) {
	t.Parallel()
	if err := ValidateReceipt(TrustedTestReceipt{}); err == nil {
		t.Fatal("empty receipt was validated")
	}
	valid := TrustedTestReceipt{
		RunID:          "run-1",
		Profile:        KVMSuiteProfile,
		CandidateSHA:   strings.Repeat("a", 40),
		SuiteIdentity:  strings.Repeat("b", 40),
		GuestDigest:    "sha256:" + strings.Repeat("c", 64),
		ContractDigest: strings.Repeat("d", 64),
		Verdict:        VerdictPassed,
	}
	if err := ValidateReceipt(valid); err != nil {
		t.Fatalf("valid receipt rejected: %v", err)
	}
	invalid := valid
	invalid.Verdict = "bogus"
	if err := ValidateReceipt(invalid); err == nil {
		t.Fatal("bogus verdict accepted")
	}
}

func TestReceiptDigestDeterministic(t *testing.T) {
	t.Parallel()
	receipt := TrustedTestReceipt{
		RunID:          "run-1",
		Profile:        KVMSuiteProfile,
		CandidateSHA:   strings.Repeat("a", 40),
		SuiteIdentity:  strings.Repeat("b", 40),
		GuestDigest:    "sha256:" + strings.Repeat("c", 64),
		ContractDigest: strings.Repeat("d", 64),
		Verdict:        VerdictPassed,
		PodUID:         "uid-1",
		ContainerID:    "containerd://abc",
		ExitCode:       0,
		RestartCount:   0,
	}
	d1 := ReceiptDigest(receipt)
	d2 := ReceiptDigest(receipt)
	if d1 != d2 {
		t.Fatal("receipt digest is not deterministic")
	}
	different := receipt
	different.CandidateSHA = strings.Repeat("f", 40)
	if ReceiptDigest(different) == d1 {
		t.Fatal("different receipts have same digest")
	}
}

func TestTerminationMessageHash(t *testing.T) {
	t.Parallel()
	h1 := TerminationMessageHash(`{"cases":3,"passed":3}`)
	h2 := TerminationMessageHash(`{"cases":3,"passed":3}`)
	if h1 != h2 {
		t.Fatal("same message produces different hashes")
	}
	if len(h1) != 64 {
		t.Fatalf("hash length = %d, want 64", len(h1))
	}
	if TerminationMessageHash("different") == h1 {
		t.Fatal("different messages produce same hash")
	}
}
