package vmrunner

import (
	"os"
	"testing"
	"time"
)

// TestDecodeEbpfConsumerContract validates the prepared cloudtaser-ebpf
// .oberth/tests.yaml version 2 contract. The file lives in a sibling
// worktree at /var/tmp/wt/cloudtaser-ebpf/fix-278-tests-yaml-v2 and is not
// pushed until the Oberth release carrying Lane B is live. This test runs
// only when the file is present (CI will not have the sibling worktree).
func TestDecodeEbpfConsumerContract(t *testing.T) {
	t.Parallel()

	const contractPath = "/var/tmp/wt/cloudtaser-ebpf/fix-278-tests-yaml-v2/.oberth/tests.yaml"
	source, err := os.ReadFile(contractPath)
	if os.IsNotExist(err) {
		t.Skipf("ebpf consumer contract not present at %s; expected only in the local worktree", contractPath)
	}
	if err != nil {
		t.Fatal(err)
	}

	c, err := DecodeContract(source)
	if err != nil {
		t.Fatalf("DecodeContract(%s): %v", contractPath, err)
	}

	if c.Version != 2 {
		t.Errorf("version = %d, want 2", c.Version)
	}
	if c.Profile != KVMSuiteProfile {
		t.Errorf("profile = %q, want %q", c.Profile, KVMSuiteProfile)
	}
	if c.Artifact != "bpf/secret_monitor.o" {
		t.Errorf("artifact = %q, want %q", c.Artifact, "bpf/secret_monitor.o")
	}
	if c.GuestImage == "" {
		t.Error("guest_image is empty")
	}
	const wantGuestPrefix = "europe-west4-docker.pkg.dev/skipopsmain/cloudtaser/cloudtaser-kernels:"
	if len(c.GuestImage) < len(wantGuestPrefix) || c.GuestImage[:len(wantGuestPrefix)] != wantGuestPrefix {
		t.Errorf("guest_image prefix = %q, want %q...", c.GuestImage, wantGuestPrefix)
	}
	if c.Resources.CPUCores != 2 {
		t.Errorf("resources.cpu_cores = %d, want 2", c.Resources.CPUCores)
	}
	if c.Resources.MemoryMiB != 4096 {
		t.Errorf("resources.memory_mib = %d, want 4096", c.Resources.MemoryMiB)
	}
	if c.Timeout != 30*time.Minute {
		t.Errorf("timeout = %s, want 30m", c.Timeout)
	}
}
