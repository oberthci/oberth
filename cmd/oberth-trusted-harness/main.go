// Command oberth-trusted-harness is the server-owned binary that runs inside
// the trusted-tests leaf. It executes a closed test profile against a
// digest-pinned guest image using KVM acceleration, exits 0 only on an exact
// PASS, and writes a structured termination message to /dev/termination-log.
//
// This binary is stdlib-only by design: it is digest-pinned in the server
// configuration, and every dependency it carries becomes part of the trusted
// computing base for test verdicts.
//
// The harness does NOT import internal Oberth packages — it runs inside a
// separate container image, not the server image.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"time"
)

const (
	terminationLogPath = "/dev/termination-log"
	// knownProfiles is the closed vocabulary; the harness refuses unknown ones.
	profileKVMSuiteAMD64V1 = "kvm-suite-amd64-v1"
)

func main() {
	var (
		profile       string
		artifact      string
		candidateSHA  string
		suiteIdentity string
		guestImage    string
	)
	flag.StringVar(&profile, "profile", "", "closed profile name (required)")
	flag.StringVar(&artifact, "artifact", "", "artifact path relative to the source checkout (required)")
	flag.StringVar(&candidateSHA, "candidate-sha", "", "exact candidate commit SHA (40-char hex, required)")
	flag.StringVar(&suiteIdentity, "suite-identity", "", "suite revision identity (40-char hex, required)")
	flag.StringVar(&guestImage, "guest-image", "", "digest-pinned guest image reference (required)")
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	if err := run(ctx, profile, artifact, candidateSHA, suiteIdentity, guestImage); err != nil {
		writeTerminationLog("FAIL", candidateSHA, suiteIdentity, guestImage, err.Error())
		fmt.Fprintf(os.Stderr, "harness: FAIL: %v\n", err)
		os.Exit(1)
	}

	writeTerminationLog("PASS", candidateSHA, suiteIdentity, guestImage, "")
	_, _ = fmt.Fprintf(os.Stdout, "harness: PASS\n")
}

func run(ctx context.Context, profile, artifact, candidateSHA, suiteIdentity, guestImage string) error {
	// Validate inputs.
	if profile == "" || artifact == "" || candidateSHA == "" || suiteIdentity == "" || guestImage == "" {
		return fmt.Errorf("all flags are required: --profile, --artifact, --candidate-sha, --suite-identity, --guest-image")
	}
	if len(candidateSHA) != 40 {
		return fmt.Errorf("candidate-sha must be 40 hex chars, got %d", len(candidateSHA))
	}
	if len(suiteIdentity) != 40 {
		return fmt.Errorf("suite-identity must be 40 hex chars, got %d", len(suiteIdentity))
	}
	if !strings.Contains(guestImage, "@sha256:") {
		return fmt.Errorf("guest-image must be digest-pinned (@sha256:...)")
	}

	switch profile {
	case profileKVMSuiteAMD64V1:
		return runKVMSuite(ctx, artifact, guestImage)
	default:
		return fmt.Errorf("unknown profile %q; accepted: %s", profile, profileKVMSuiteAMD64V1)
	}
}

// runKVMSuite executes the kvm-suite-amd64-v1 profile. It boots the guest
// image under KVM using QEMU, passes the artifact in, and checks the exit
// code. The guest image is expected to be a bootable disk image or kernel+
// initramfs bundle that runs the test suite and exits 0 on pass.
func runKVMSuite(ctx context.Context, artifact, guestImage string) error {
	// Verify KVM is available — the server should have injected the device,
	// but the harness must not silently fall back to TCG.
	if _, err := os.Stat("/dev/kvm"); err != nil {
		return fmt.Errorf("KVM device not available: %w; the trusted-tests leaf requires -accel kvm", err)
	}

	// Verify the artifact exists.
	if _, err := os.Stat(artifact); err != nil {
		return fmt.Errorf("artifact not found: %w", err)
	}

	// The guest image reference is a container image digest. The actual disk
	// image or kernel bundle must be extracted from it. For the initial
	// implementation, the harness expects the guest image to have been
	// pre-pulled and extracted to /work/guest/ by an init container.
	guestDir := "/work/guest"
	if _, err := os.Stat(guestDir); err != nil {
		return fmt.Errorf("guest directory %s not found: the guest image must be extracted before the harness runs", guestDir)
	}

	// Look for a kernel and initramfs in the guest directory.
	kernelPath := guestDir + "/vmlinuz"
	initramfsPath := guestDir + "/initramfs.img"

	if _, err := os.Stat(kernelPath); err != nil {
		return fmt.Errorf("guest kernel not found at %s: %w", kernelPath, err)
	}

	// Build the QEMU command. The harness enforces -accel kvm only — no TCG
	// fallback under any circumstances.
	args := []string{
		"-accel", "kvm",
		"-m", "2048",
		"-smp", "2",
		"-nographic",
		"-no-reboot",
		"-kernel", kernelPath,
		"-append", fmt.Sprintf("console=ttyS0 oberth.artifact=%s oberth.profile=%s", artifact, profileKVMSuiteAMD64V1),
	}

	// Add initramfs if present.
	if _, err := os.Stat(initramfsPath); err == nil {
		args = append(args, "-initrd", initramfsPath)
	}

	// Find qemu-system-x86_64 in PATH.
	qemuBin, err := exec.LookPath("qemu-system-x86_64")
	if err != nil {
		return fmt.Errorf("qemu-system-x86_64 not found: %w; the harness image must include QEMU", err)
	}

	fmt.Fprintf(os.Stderr, "harness: starting QEMU: %s %s\n", qemuBin, strings.Join(args, " "))
	start := time.Now()

	cmd := exec.CommandContext(ctx, qemuBin, args...) // #nosec G204 -- args are harness-controlled, not user input
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil {
		elapsed := time.Since(start).Truncate(time.Second)
		return fmt.Errorf("QEMU exited with error after %s: %w", elapsed, err)
	}

	elapsed := time.Since(start).Truncate(time.Second)
	fmt.Fprintf(os.Stderr, "harness: QEMU completed successfully in %s\n", elapsed)
	return nil
}

func writeTerminationLog(verdict, candidateSHA, suiteIdentity, guestImage, errMsg string) {
	guestDigest := guestImage
	if idx := strings.LastIndex(guestDigest, "@"); idx >= 0 {
		guestDigest = guestDigest[idx+1:]
	}

	msg := fmt.Sprintf("verdict=%s candidate=%s suite=%s guest=%s",
		verdict, candidateSHA, suiteIdentity, guestDigest)
	if errMsg != "" {
		// Truncate error to fit within the 4096-byte termination message limit.
		maxErr := 4096 - len(msg) - len(" error=") - 1
		if maxErr > 0 && len(errMsg) > maxErr {
			errMsg = errMsg[:maxErr]
		}
		msg += " error=" + errMsg
	}

	// Best effort — the kubelet reads this file after the container exits.
	_ = os.WriteFile(terminationLogPath, []byte(msg), 0600) // #nosec G306
}
