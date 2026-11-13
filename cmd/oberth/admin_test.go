package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	cryptorand "crypto/rand"
	"encoding/pem"
	"errors"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/testtimeout"
)

// TestUpstreamListShowsPerUpstreamKeyFingerprints pins the multi-key listing
// contract: an upstream with a dedicated key name resolves its fingerprint
// from the projected per-upstream file, a shared-key upstream resolves the
// shared file, the two fingerprints differ, and only fingerprints — never key
// material — appear in the output.

// TestUpstreamListDegradesUnreadableDedicatedKey pins the degrade contract:
// a dedicated key name whose file is not projected yet lists "-" instead of
// silently displaying the shared key's fingerprint as if it were dedicated.

func TestAdminMutationGateUsesLiveDaemonSocket(t *testing.T) {
	socket := privateAdminSocket(t)
	if err := validateAdminSocket(socket); err != nil {
		t.Fatal(err)
	}
	called := make(chan struct{}, 1)
	server := newAdminGateServer(func(context.Context) error {
		called <- struct{}{}
		return nil
	}, "/data/oberth.sqlite", nil)
	listener, err := listenAdminSocket(context.Background(), socket)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = server.Close()
		_ = listener.Close()
		_ = removeStaleAdminSocket(socket)
	})
	serveDone := make(chan error, 1)
	go func() { serveDone <- server.Serve(listener) }()
	if err := requestAdminMutationGate(context.Background(), socket, "uplink.register", "/data/oberth.sqlite"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-called:
	case <-time.After(testtimeout.Start(time.Second)):
		t.Fatal("live daemon mutation gate was not invoked")
	}
	if err := requestAdminMutationGate(context.Background(), socket, "uplink.register", "/tmp/rolled-back.sqlite"); err == nil ||
		!strings.Contains(err.Error(), "not the live daemon database") {
		t.Fatalf("mismatched database gate error = %v", err)
	}
	select {
	case <-called:
		t.Fatal("daemon gate ran for a non-live database target")
	default:
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), testtimeout.Start(time.Second))
	defer cancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		t.Fatal(err)
	}
	if err := <-serveDone; !errors.Is(err, http.ErrServerClosed) {
		t.Fatalf("admin server exit = %v", err)
	}
	if err := removeStaleAdminSocket(socket); err != nil {
		t.Fatal(err)
	}
}

// TestUpstreamAddWaitsForKeyRegistrationInOneInvocation is the issue 007
// acceptance path: generate, print, wait while the operator registers the key
// in another tab, then complete registration — one command, no rerun.

// TestUpstreamAddRegistrationWaitCancelsCleanly pins the Ctrl+C contract:
// cancellation stops the wait, keeps the persisted key, exits zero, and
// prints the rerun fallback.

// TestUpstreamAddStoresPerUpstreamDataKeys pins the multi-forge key
// contract: a new SSH upstream gets a dedicated Ed25519 identity stored as
// additional data keys of the one chart-owned upstream-key Secret, applied
// under a per-upstream server-side-apply field manager so one upstream's
// registration can never prune another upstream's key material, and the
// registered row records the dedicated key name.

// TestUpstreamAddRejectsUnsafeNames pins ValidateUpstreamName at the CLI
// boundary: the name becomes a Secret data key, a projected file name, and an
// SSH config identity path element, so everything outside a DNS-1123 label is
// refused before any side effect.
// TestUpstreamAddHelpIncludesExpectedHostFingerprint pins the flag visibility:
// the help text for upstream add must advertise --expected-host-fingerprint so
// the error message's instruction is actionable.

// TestUpstreamAddExpectedHostFingerprintFlagPlumbed proves that
// --expected-host-fingerprint is wired through to UpstreamSSHBootstrap: a
// mismatched fingerprint produces the fingerprint-specific error rather than
// the generic "--yes cannot trust" error that would appear if the flag were
// not plumbed.

// --- #445: uplink add error improvements ---

func testSSHIdentity(t *testing.T) (ssh.PublicKey, []byte, []byte) {
	t.Helper()
	publicKey, privateKey, err := ed25519.GenerateKey(cryptorand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	sshKey, err := ssh.NewPublicKey(publicKey)
	if err != nil {
		t.Fatal(err)
	}
	block, err := ssh.MarshalPrivateKey(privateKey, "test")
	if err != nil {
		t.Fatal(err)
	}
	return sshKey, pem.EncodeToMemory(block), ssh.MarshalAuthorizedKey(sshKey)
}

// captureInteractiveRunner returns a runInteractive stub that records the
// command and args it was called with instead of executing anything.

// capturedStdinExec records a command invocation with its stdin content.

// captureStdinRunner returns a runWithStdin stub that records the command,
// args, and stdin content instead of executing anything.

// TestHostModeUpstreamAddGenerateAutoYes pins the host-mode --yes path:
// the [G]enerate / [P]rovide prompt is skipped, and the command execs into
// the pod with --yes for auto-generation.

// TestHostModeUpstreamAddGenerateInteractive pins the host-mode interactive
// path: the user is prompted with [G]enerate / [P]rovide and chooses G.
// The command execs into the pod interactively (no --yes) for generation.

// TestHostModeUpstreamAddProvide pins the host-mode Provide path: the user
// supplies an existing private key, which is streamed to the pod via a
// kubectl exec provide-key invocation. The command then execs into the pod
// with --yes for the main add flow (the persisted key is found by the pod).

// TestHostModeUpstreamAddProvideWithDedicatedKey pins the --dedicated-key
// flag in host-mode Provide: the provide-key exec includes the flag and the
// main add exec includes it.

// TestHostModeUpstreamAddProvideRejectsPassphraseKey pins the passphrase
// rejection in the host-mode Provide path — local validation catches it
// before any exec.

// TestHostModeUpstreamAddProvideRejectsNonexistentFile pins the error when
// the user provides a path to a file that does not exist.

// TestHostModeUpstreamAddProvideRefusesOverwrite pins that a pod-side
// overwrite rejection propagates back through the host Provide path. The
// overwrite check itself is in the pod's provide-key handler; this test
// verifies the host flow surfaces the error (tested end-to-end by
// TestInPodProvideKeyRefusesOverwrite).

// TestInPodProvideKeyFailsClosedWithoutMutationGate verifies that the in-pod
// provide-key handler refuses to apply a Secret when the audit mutation gate
// is unavailable — mirroring TestUpstreamSecretMutationFailsClosedBeforePatch.

// TestInPodProvideKeyRefusesOverwrite verifies that the in-pod provide-key
// handler refuses to overwrite an existing data key in the upstream Secret.

// TestHostModeNeverDialsAdminSocket pins the structural invariant that the
// host-mode add flow never dials the pod-private admin mutation gate socket.
// The hostUpstreamDeps struct has no mutationGate field, so the only code
// paths are kubectl exec invocations — asserted here.

// TestHostProvideRejectsGroupAccessibleKeyFile pins the permission parity
// between host-mode Provide and in-pod validateOperatorFile: key files that
// are writable by group or accessible by other are refused.

// TestUpstreamAddHelpNeedsNoKubeconfig verifies that `upstream add --help`
// prints usage and returns nil even on a host with no kubeconfig (item 2).
func TestUpstreamAddHelpNeedsNoKubeconfig(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("KUBECONFIG", filepath.Join(t.TempDir(), "nonexistent"))
	// Force host mode by unsetting the in-cluster env.
	t.Setenv("KUBERNETES_SERVICE_HOST", "")
	t.Setenv("KUBERNETES_SERVICE_PORT", "")
	var output bytes.Buffer
	err := runUpstream(context.Background(), []string{"add", "--help"}, &output)
	if err != nil {
		t.Fatalf("upstream add --help with no kubeconfig = %v", err)
	}
	if !strings.Contains(output.String(), "upstream") {
		t.Fatalf("expected usage output, got %q", output.String())
	}
}

// TestHostProvideKeyStreamingReachesPod verifies that the host Provide path
// calls the provide-key subcommand via kubectl exec with the key material
// piped on stdin, and that the exec args include all required flags.

// TestHostModeTargetDisclosure verifies that host-mode add prints the
// kubeconfig target before the first mutating action.

// TestInPodModeSkipsKeySelectionPrompt pins that in-pod mode
// (runUpstreamWithDependencies) does NOT show the [G]enerate / [P]rovide
// prompt. This preserves the existing behavior where the key is always
// generated in-pod.

// TestHostModeExecPassesThroughFlags pins that the kubectl exec command
// correctly passes through --dedicated-key, --expected-host-fingerprint,
// --no-wait and --repin.

// uplink rotate registers the new key for the identity and revokes the old
// one, so replacing a workstation key is one step instead of add + remove.

func TestUpstreamAuthStatusReportsWithoutProbingNonSSH(t *testing.T) {
	t.Parallel()
	missing := filepath.Join(t.TempDir(), "absent")
	if got := upstreamAuthStatus(context.Background(), model.Upstream{Kind: "local"}, "", missing, t.TempDir(), missing); got != "-" {
		t.Fatalf("local upstream: %q", got)
	}
	if got := upstreamAuthStatus(context.Background(), model.Upstream{Kind: "ssh", BaseURL: "ssh://git@example.test/acme"}, "", missing, t.TempDir(), missing); got != "error: read upstream identity" {
		t.Fatalf("missing key: %q", got)
	}
}
