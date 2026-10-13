package argojob

import (
	"errors"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/oberthci/oberth/internal/vmrunner"
)

func terminatedPod(exitCode int32, restartCount int32, message string) *corev1.Pod {
	finished := metav1.Now()
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			UID: types.UID("pod-uid-123"),
		},
		Status: corev1.PodStatus{
			ContainerStatuses: []corev1.ContainerStatus{
				{
					Name:         "main",
					ContainerID:  "containerd://abc123",
					RestartCount: restartCount,
					State: corev1.ContainerState{
						Terminated: &corev1.ContainerStateTerminated{
							ExitCode:   exitCode,
							Message:    message,
							FinishedAt: finished,
						},
					},
				},
			},
		},
	}
}

func TestExtractKubeletRecordPassing(t *testing.T) {
	t.Parallel()
	pod := terminatedPod(0, 0, `{"cases":3,"passed":3}`)
	record, err := extractKubeletRecord(pod)
	if err != nil {
		t.Fatal(err)
	}
	if record.PodUID != "pod-uid-123" {
		t.Fatalf("pod UID = %q", record.PodUID)
	}
	if record.ContainerID != "containerd://abc123" {
		t.Fatalf("container ID = %q", record.ContainerID)
	}
	if record.ExitCode != 0 {
		t.Fatalf("exit code = %d", record.ExitCode)
	}
	if record.RestartCount != 0 {
		t.Fatalf("restart count = %d", record.RestartCount)
	}
}

func TestExtractKubeletRecordExitNonzero(t *testing.T) {
	t.Parallel()
	pod := terminatedPod(1, 0, "")
	record, err := extractKubeletRecord(pod)
	if err != nil {
		t.Fatal(err)
	}
	if record.ExitCode != 1 {
		t.Fatalf("exit code = %d, want 1", record.ExitCode)
	}
}

func TestExtractKubeletRecordRestart(t *testing.T) {
	t.Parallel()
	pod := terminatedPod(0, 1, "")
	record, err := extractKubeletRecord(pod)
	if err != nil {
		t.Fatal(err)
	}
	if record.RestartCount != 1 {
		t.Fatalf("restart count = %d, want 1", record.RestartCount)
	}
}

func TestExtractKubeletRecordNotTerminal(t *testing.T) {
	t.Parallel()
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{UID: types.UID("running-pod")},
		Status: corev1.PodStatus{
			ContainerStatuses: []corev1.ContainerStatus{
				{Name: "main", State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}},
			},
		},
	}
	_, err := extractKubeletRecord(pod)
	if !errors.Is(err, ErrTrustedLeafNotTerminal) {
		t.Fatalf("error = %v, want ErrTrustedLeafNotTerminal", err)
	}
}

func TestBuildTrustedReceiptPassingCase(t *testing.T) {
	t.Parallel()
	contract := vmrunner.Contract{
		Version:    2,
		Profile:    vmrunner.KVMSuiteProfile,
		Artifact:   vmrunner.BPFArtifact,
		GuestImage: "example.com/guest@sha256:" + strings.Repeat("b", 64),
		Resources:  vmrunner.ContractResources{CPUCores: 2, MemoryMiB: 4096},
		Timeout:    30 * time.Minute,
	}
	start := time.Now().Add(-5 * time.Minute)
	record := vmrunner.KubeletRecord{
		PodUID:       "uid-123",
		ContainerID:  "containerd://abc",
		ExitCode:     0,
		RestartCount: 0,
		Message:      `{"cases":3,"passed":3}`,
		FinishedAt:   time.Now(),
	}

	receipt := BuildTrustedReceipt(
		"run-1", contract, strings.Repeat("d", 40),
		vmrunner.ContractDigest(&contract), record, start,
	)
	if receipt.Verdict != vmrunner.VerdictPassed {
		t.Fatalf("verdict = %s, want passed", receipt.Verdict)
	}
	if receipt.PodUID != "uid-123" {
		t.Fatalf("pod UID = %q", receipt.PodUID)
	}
	if receipt.RefusalReason != "" {
		t.Fatalf("refusal reason = %q, want empty", receipt.RefusalReason)
	}
}

func TestBuildTrustedReceiptExitNonzero(t *testing.T) {
	t.Parallel()
	contract := vmrunner.Contract{
		Version:    2,
		Profile:    vmrunner.KVMSuiteProfile,
		Artifact:   vmrunner.BPFArtifact,
		GuestImage: "example.com/guest@sha256:" + strings.Repeat("b", 64),
		Resources:  vmrunner.ContractResources{CPUCores: 2, MemoryMiB: 4096},
		Timeout:    30 * time.Minute,
	}
	record := vmrunner.KubeletRecord{
		PodUID:      "uid-123",
		ContainerID: "containerd://abc",
		ExitCode:    1,
		FinishedAt:  time.Now(),
	}

	receipt := BuildTrustedReceipt(
		"run-1", contract, strings.Repeat("d", 40),
		vmrunner.ContractDigest(&contract), record, time.Now().Add(-5*time.Minute),
	)
	if receipt.Verdict != vmrunner.VerdictRefused {
		t.Fatalf("verdict = %s, want refused", receipt.Verdict)
	}
	if !strings.Contains(receipt.RefusalReason, "exit_nonzero") {
		t.Fatalf("reason = %q, want exit_nonzero", receipt.RefusalReason)
	}
}

func TestBuildTrustedReceiptRestartDetected(t *testing.T) {
	t.Parallel()
	contract := vmrunner.Contract{
		Version:    2,
		Profile:    vmrunner.KVMSuiteProfile,
		Artifact:   vmrunner.BPFArtifact,
		GuestImage: "example.com/guest@sha256:" + strings.Repeat("b", 64),
		Resources:  vmrunner.ContractResources{CPUCores: 2, MemoryMiB: 4096},
		Timeout:    30 * time.Minute,
	}
	record := vmrunner.KubeletRecord{
		PodUID:       "uid-123",
		ContainerID:  "containerd://abc",
		ExitCode:     0,
		RestartCount: 1,
		FinishedAt:   time.Now(),
	}

	receipt := BuildTrustedReceipt(
		"run-1", contract, strings.Repeat("d", 40),
		vmrunner.ContractDigest(&contract), record, time.Now().Add(-5*time.Minute),
	)
	if receipt.Verdict != vmrunner.VerdictRefused {
		t.Fatalf("verdict = %s, want refused", receipt.Verdict)
	}
	if !strings.Contains(receipt.RefusalReason, "restart_detected") {
		t.Fatalf("reason = %q, want restart_detected", receipt.RefusalReason)
	}
}

func TestBuildTrustedReceiptTimeout(t *testing.T) {
	t.Parallel()
	contract := vmrunner.Contract{
		Version:    2,
		Profile:    vmrunner.KVMSuiteProfile,
		Artifact:   vmrunner.BPFArtifact,
		GuestImage: "example.com/guest@sha256:" + strings.Repeat("b", 64),
		Resources:  vmrunner.ContractResources{CPUCores: 2, MemoryMiB: 4096},
		Timeout:    10 * time.Minute,
	}
	start := time.Now().Add(-15 * time.Minute)
	record := vmrunner.KubeletRecord{
		PodUID:      "uid-123",
		ContainerID: "containerd://abc",
		ExitCode:    0,
		FinishedAt:  time.Now(),
	}

	receipt := BuildTrustedReceipt(
		"run-1", contract, strings.Repeat("d", 40),
		vmrunner.ContractDigest(&contract), record, start,
	)
	if receipt.Verdict != vmrunner.VerdictRefused {
		t.Fatalf("verdict = %s, want refused", receipt.Verdict)
	}
	if !strings.Contains(receipt.RefusalReason, "timeout") {
		t.Fatalf("reason = %q, want timeout", receipt.RefusalReason)
	}
}
