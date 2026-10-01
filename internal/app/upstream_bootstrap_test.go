package app

import (
	"bytes"
	"context"
	"crypto/ed25519"
	cryptorand "crypto/rand"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"

	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"
)

func TestAcquireHostKeysAcceptsPublishedFingerprintForWellKnownForge(t *testing.T) {
	t.Parallel()
	githubKey := parseAuthorizedKeyForTest(t, "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOMqqnkVzrm0SdG6UOoqKLsabgH5C9okWi0dh2l9GKJl")
	var output bytes.Buffer
	bootstrap := UpstreamSSHBootstrap{
		Output: &output,
		ScanHostKeys: func(context.Context, string, string) ([]ssh.PublicKey, error) {
			return []ssh.PublicKey{githubKey}, nil
		},
	}

	keys, err := bootstrap.acquireHostKeys(
		context.Background(),
		newConfirmationReader(strings.NewReader("")),
		"GitHub.com",
		"22",
	)
	if err != nil {
		t.Fatalf("acquireHostKeys: %v", err)
	}
	if len(keys) != 1 {
		t.Fatalf("acquireHostKeys returned %d keys, want 1", len(keys))
	}
	if output.Len() != 0 {
		t.Fatalf("well-known forge unexpectedly prompted for confirmation: %q", output.String())
	}
}

func TestAcquireHostKeysAcceptsPublishedFingerprintWithTrailingRootDot(t *testing.T) {
	t.Parallel()
	githubKey := parseAuthorizedKeyForTest(t, "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOMqqnkVzrm0SdG6UOoqKLsabgH5C9okWi0dh2l9GKJl")
	var output bytes.Buffer
	var scannedHost string
	bootstrap := UpstreamSSHBootstrap{
		Output: &output,
		ScanHostKeys: func(_ context.Context, host, _ string) ([]ssh.PublicKey, error) {
			scannedHost = host
			return []ssh.PublicKey{githubKey}, nil
		},
	}

	keys, err := bootstrap.acquireHostKeys(
		context.Background(),
		newConfirmationReader(strings.NewReader("")),
		"GitHub.com.",
		"22",
	)
	if err != nil {
		t.Fatalf("acquireHostKeys: %v", err)
	}
	if len(keys) != 1 {
		t.Fatalf("acquireHostKeys returned %d keys, want 1", len(keys))
	}
	if scannedHost != "GitHub.com." {
		t.Fatalf("ScanHostKeys host = %q, want %q", scannedHost, "GitHub.com.")
	}
	if output.Len() != 0 {
		t.Fatalf("well-known forge unexpectedly prompted for confirmation: %q", output.String())
	}
}

func TestAcquireHostKeysRejectsAnyMismatchedFingerprintForWellKnownForge(t *testing.T) {
	t.Parallel()
	githubKey := parseAuthorizedKeyForTest(t, "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOMqqnkVzrm0SdG6UOoqKLsabgH5C9okWi0dh2l9GKJl")
	attackerPublic, _, err := ed25519.GenerateKey(cryptorand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	attackerKey, err := ssh.NewPublicKey(attackerPublic)
	if err != nil {
		t.Fatal(err)
	}
	bootstrap := UpstreamSSHBootstrap{
		Output: io.Discard,
		ScanHostKeys: func(context.Context, string, string) ([]ssh.PublicKey, error) {
			return []ssh.PublicKey{githubKey, attackerKey}, nil
		},
	}

	_, err = bootstrap.acquireHostKeys(
		context.Background(),
		newConfirmationReader(strings.NewReader("yes\n")),
		"github.com",
		"22",
	)
	want := "MITM WARNING: github.com SSH fingerprint does not match published value"
	if err == nil || err.Error() != want {
		t.Fatalf("acquireHostKeys error = %v, want %q", err, want)
	}
}

func TestAcquireHostKeysRejectsMismatchedFingerprintWithTrailingRootDot(t *testing.T) {
	t.Parallel()
	attackerPublic, _, err := ed25519.GenerateKey(cryptorand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	attackerKey, err := ssh.NewPublicKey(attackerPublic)
	if err != nil {
		t.Fatal(err)
	}
	bootstrap := UpstreamSSHBootstrap{
		Output: io.Discard,
		ScanHostKeys: func(context.Context, string, string) ([]ssh.PublicKey, error) {
			return []ssh.PublicKey{attackerKey}, nil
		},
	}

	_, err = bootstrap.acquireHostKeys(
		context.Background(),
		newConfirmationReader(strings.NewReader("yes\n")),
		"github.com.",
		"22",
	)
	want := "MITM WARNING: github.com. SSH fingerprint does not match published value"
	if err == nil || err.Error() != want {
		t.Fatalf("acquireHostKeys error = %v, want %q", err, want)
	}
}

func TestAcquireHostKeysStillPromptsForUnknownForge(t *testing.T) {
	t.Parallel()
	public, _, err := ed25519.GenerateKey(cryptorand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	key, err := ssh.NewPublicKey(public)
	if err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	bootstrap := UpstreamSSHBootstrap{
		Output: &output,
		ScanHostKeys: func(context.Context, string, string) ([]ssh.PublicKey, error) {
			return []ssh.PublicKey{key}, nil
		},
	}

	if _, err := bootstrap.acquireHostKeys(
		context.Background(),
		newConfirmationReader(strings.NewReader("yes\n")),
		"forge.example",
		"22",
	); err != nil {
		t.Fatalf("acquireHostKeys: %v", err)
	}
	if !strings.Contains(output.String(), ssh.FingerprintSHA256(key)) {
		t.Fatalf("confirmation output did not include scanned fingerprint: %q", output.String())
	}
}

func TestWellKnownForgeFingerprintsMatchPublishedValues(t *testing.T) {
	t.Parallel()
	expected := map[string][]string{
		"github.com": {
			"SHA256:+DiY3wvvV6TuJJhbpZisF/zLDA0zPMSvHdkr4UvCOqU",
			"SHA256:uNiVztksCsDhcc0u9e8BujQXVUpKZIDTMczCvj3tD2s",
			"SHA256:p2QAMXNIC1TJYWeIOttrVc98/R1BUFWu3/LiyKgUfQM",
		},
		"codeberg.org": {
			"SHA256:mIlxA9k46MmM6qdJOdMnAQpzGxF4WIVVL+fj+wZbw0g",
			"SHA256:6QQmYi4ppFS4/+zSZ5S4IU+4sa6rwvQ4PbhCtPEBekQ",
			"SHA256:T9FYDEHELhVkulEKKwge5aVhVTbqCW0MIRwAfpARs/E",
		},
		"gitlab.com": {
			"SHA256:eUXGGm1YGsMAS7vkcx6JOJdOGHPem5gQp4taiCfCLB8",
			"SHA256:ROQFvPThGrW4RuWLoL9tq9I9zJ42fK4XywyRtbOz/EQ",
			"SHA256:HbW3g8zUjNSksFbqTiUWPWg2Bq1x8xdGUrliXFzSnUw",
		},
		"bitbucket.org": {
			"SHA256:ybgmFkzwOSotHTHLJgHO0QN8L0xErw6vd0VhFA9m3SM",
			"SHA256:46OSHA1Rmj8E8ERTC6xkNcmGOw9oFxYr0WF6zWW8l1E",
			"SHA256:FC73VB6C4OQLSCrjEayhMp9UMxS97caD/Yyi2bhW/J0",
		},
	}

	if len(wellKnownForges) != len(expected) {
		t.Fatalf("wellKnownForges contains %d hosts, want %d", len(wellKnownForges), len(expected))
	}
	for host, fingerprints := range expected {
		published, ok := wellKnownForges[host]
		if !ok {
			t.Errorf("wellKnownForges is missing %s", host)
			continue
		}
		if len(published) != len(fingerprints) {
			t.Errorf("wellKnownForges[%q] contains %d fingerprints, want %d", host, len(published), len(fingerprints))
		}
		for _, fingerprint := range fingerprints {
			if _, ok := published[fingerprint]; !ok {
				t.Errorf("wellKnownForges[%q] is missing %s", host, fingerprint)
			}
		}
	}
}

func parseAuthorizedKeyForTest(t *testing.T, value string) ssh.PublicKey {
	t.Helper()
	key, _, _, _, err := ssh.ParseAuthorizedKey([]byte(value))
	if err != nil {
		t.Fatal(err)
	}
	return key
}

// TestProbeSSHAuthenticationHandshakesWithoutOpeningChannels proves that the
// probe completes a full SSH handshake (host-key verification + publickey
// authentication) and returns nil — without opening any channel or executing
// any command. The fake SSH server FAILs the test if any channel-open request
// arrives, confirming the probe cannot mutate or interrogate the forge.
func TestProbeSSHAuthenticationHandshakesWithoutOpeningChannels(t *testing.T) {
	t.Parallel()
	clientPublic, clientPrivate, err := ed25519.GenerateKey(cryptorand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	clientSSHKey, err := ssh.NewPublicKey(clientPublic)
	if err != nil {
		t.Fatal(err)
	}
	hostPublic, hostPrivate, err := ed25519.GenerateKey(cryptorand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	hostSigner, err := ssh.NewSignerFromKey(hostPrivate)
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })

	serverErrors := make(chan error, 1)
	go serveAuthProbe(t, listener, hostSigner, clientSSHKey, serverErrors)

	privateBlock, err := ssh.MarshalPrivateKey(clientPrivate, "test")
	if err != nil {
		t.Fatal(err)
	}
	hostKey, err := ssh.NewPublicKey(hostPublic)
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	knownHosts := []byte(knownhosts.Line([]string{address}, hostKey) + "\n")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := ProbeSSHAuthentication(ctx, "ssh://git@"+address+"/acme", pem.EncodeToMemory(privateBlock), knownHosts); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-serverErrors:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
}

// TestProbeSSHAuthenticationRejectionSatisfiesClassifier verifies that a
// server-rejected key produces an error that isExpectedAuthRejection classifies
// as the normal "not registered yet" state — the exact error path the
// waitForRegistration loop depends on for silent dot output.
func TestProbeSSHAuthenticationRejectionSatisfiesClassifier(t *testing.T) {
	t.Parallel()
	_, clientPrivate, err := ed25519.GenerateKey(cryptorand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	hostPublic, hostPrivate, err := ed25519.GenerateKey(cryptorand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	hostSigner, err := ssh.NewSignerFromKey(hostPrivate)
	if err != nil {
		t.Fatal(err)
	}
	// Server accepts no keys — every connection gets an auth rejection.
	rejectAll := &ssh.ServerConfig{
		PublicKeyCallback: func(ssh.ConnMetadata, ssh.PublicKey) (*ssh.Permissions, error) {
			return nil, fmt.Errorf("key not registered")
		},
	}
	rejectAll.AddHostKey(hostSigner)

	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })

	go func() {
		conn, acceptErr := listener.Accept()
		if acceptErr != nil {
			return
		}
		defer conn.Close()
		_, _, _, _ = ssh.NewServerConn(conn, rejectAll)
	}()

	privateBlock, err := ssh.MarshalPrivateKey(clientPrivate, "test")
	if err != nil {
		t.Fatal(err)
	}
	hostKey, err := ssh.NewPublicKey(hostPublic)
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	knownHosts := []byte(knownhosts.Line([]string{address}, hostKey) + "\n")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	probeErr := ProbeSSHAuthentication(ctx, "ssh://git@"+address+"/org", pem.EncodeToMemory(privateBlock), knownHosts)
	if probeErr == nil {
		t.Fatal("probe succeeded against a reject-all server")
	}
	if !isExpectedAuthRejection(probeErr) {
		t.Fatalf("probe error %q was not classified as an expected auth rejection", probeErr)
	}
}

// serveAuthProbe accepts one SSH connection, authenticates the client, and
// then waits for the client to disconnect. It fails the test if any channel
// is opened — the authentication probe must never open sessions or execute
// commands.
func serveAuthProbe(t *testing.T, listener net.Listener, hostSigner ssh.Signer, allowedKey ssh.PublicKey, result chan<- error) {
	t.Helper()
	connection, err := listener.Accept()
	if err != nil {
		result <- err
		return
	}
	defer connection.Close()
	configuration := &ssh.ServerConfig{
		PublicKeyCallback: func(metadata ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
			if metadata.User() != "git" || !strings.EqualFold(ssh.FingerprintSHA256(key), ssh.FingerprintSHA256(allowedKey)) {
				return nil, fmt.Errorf("unexpected probe identity %q", metadata.User())
			}
			return nil, nil
		},
	}
	configuration.AddHostKey(hostSigner)
	server, channels, requests, err := ssh.NewServerConn(connection, configuration)
	if err != nil {
		result <- err
		return
	}
	defer server.Close()
	go ssh.DiscardRequests(requests)
	// The probe must never open a channel. If the channels chan closes (client
	// disconnected cleanly), that is the expected outcome. Any channel-open
	// request is a test failure.
	for incoming := range channels {
		result <- fmt.Errorf("probe opened an unexpected %q channel", incoming.ChannelType())
		return
	}
	result <- nil
}

func TestApplySecretForcesOwnershipOfHelmClaimedFields(t *testing.T) {
	t.Parallel()
	clientset := fake.NewClientset()
	bootstrap := UpstreamSSHBootstrap{
		Namespace: "oberth",
		MutationGate: func(context.Context, string) error {
			return nil
		},
	}
	if err := bootstrap.applySecret(context.Background(), clientset, "oberth-known-hosts", map[string][]byte{
		"known_hosts": []byte("codeberg.org ssh-ed25519 AAAA"),
	}, ""); err != nil {
		t.Fatalf("applySecret: %v", err)
	}
	var patches []clienttesting.PatchActionImpl
	for _, action := range clientset.Actions() {
		if patch, ok := action.(clienttesting.PatchActionImpl); ok {
			patches = append(patches, patch)
		}
	}
	if len(patches) != 1 {
		t.Fatalf("expected exactly one patch action, got %d", len(patches))
	}
	patch := patches[0]
	if patch.PatchType != types.ApplyPatchType {
		t.Fatalf("expected server-side apply, got %q", patch.PatchType)
	}
	if patch.PatchOptions.FieldManager != bootstrapFieldManager {
		t.Fatalf("expected field manager %q, got %q", bootstrapFieldManager, patch.PatchOptions.FieldManager)
	}
	// The chart's zero-prerequisite install pre-creates the upstream Secrets
	// with helm owning .data; a non-forced apply fails closed against that
	// placeholder ownership and breaks the first `oberth upstream add`.
	if patch.PatchOptions.Force == nil || !*patch.PatchOptions.Force {
		t.Fatal("bootstrap server-side apply must force ownership of its fields")
	}
}

func TestApplySecretRefusesWithoutMutationGate(t *testing.T) {
	t.Parallel()
	clientset := fake.NewSimpleClientset()
	bootstrap := UpstreamSSHBootstrap{Namespace: "oberth"}
	err := bootstrap.applySecret(context.Background(), clientset, "oberth-known-hosts", nil, "")
	if err == nil {
		t.Fatal("expected applySecret to fail without a mutation gate")
	}
	if len(clientset.Actions()) != 0 {
		t.Fatalf("expected no API actions without a mutation gate, got %d", len(clientset.Actions()))
	}
}

func TestAcquireHostKeysAutoYesRefusedForUnknownForge(t *testing.T) {
	t.Parallel()
	public, _, err := ed25519.GenerateKey(cryptorand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	key, err := ssh.NewPublicKey(public)
	if err != nil {
		t.Fatal(err)
	}
	bootstrap := UpstreamSSHBootstrap{
		Output:  io.Discard,
		AutoYes: true,
		ScanHostKeys: func(context.Context, string, string) ([]ssh.PublicKey, error) {
			return []ssh.PublicKey{key}, nil
		},
	}

	_, err = bootstrap.acquireHostKeys(
		context.Background(),
		newConfirmationReader(strings.NewReader("yes\n")),
		"forge.example",
		"22",
	)
	if err == nil {
		t.Fatal("--yes alone must not auto-accept unknown forge host keys")
	}
	if !strings.Contains(err.Error(), "--yes cannot trust") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestAcquireHostKeysExpectedFingerprintMatchesScanResult(t *testing.T) {
	t.Parallel()
	public, _, err := ed25519.GenerateKey(cryptorand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	key, err := ssh.NewPublicKey(public)
	if err != nil {
		t.Fatal(err)
	}
	fingerprint := ssh.FingerprintSHA256(key)
	bootstrap := UpstreamSSHBootstrap{
		Output:                  io.Discard,
		AutoYes:                 true,
		ExpectedHostFingerprint: fingerprint,
		ScanHostKeys: func(context.Context, string, string) ([]ssh.PublicKey, error) {
			return []ssh.PublicKey{key}, nil
		},
	}

	keys, err := bootstrap.acquireHostKeys(
		context.Background(),
		newConfirmationReader(strings.NewReader("")),
		"forge.example",
		"22",
	)
	if err != nil {
		t.Fatalf("matching fingerprint was refused: %v", err)
	}
	if len(keys) != 1 {
		t.Fatalf("got %d keys, want 1", len(keys))
	}
}

// TestAcquireHostKeysFingerprintPathPinsOnlyMatchedKey pins the security
// invariant: when ExpectedHostFingerprint matches one scanned key, only that
// key is returned for persistence — an attacker-injected key with a different
// fingerprint must be excluded, preventing durable trust injection via a
// network position that can present the legitimate public key (which is public)
// alongside a rogue key.
func TestAcquireHostKeysFingerprintPathPinsOnlyMatchedKey(t *testing.T) {
	t.Parallel()
	legitPublic, _, err := ed25519.GenerateKey(cryptorand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	legitKey, err := ssh.NewPublicKey(legitPublic)
	if err != nil {
		t.Fatal(err)
	}
	attackerPublic, _, err := ed25519.GenerateKey(cryptorand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	attackerKey, err := ssh.NewPublicKey(attackerPublic)
	if err != nil {
		t.Fatal(err)
	}
	fingerprint := ssh.FingerprintSHA256(legitKey)
	bootstrap := UpstreamSSHBootstrap{
		Output:                  io.Discard,
		AutoYes:                 true,
		ExpectedHostFingerprint: fingerprint,
		ScanHostKeys: func(context.Context, string, string) ([]ssh.PublicKey, error) {
			return []ssh.PublicKey{attackerKey, legitKey}, nil
		},
	}

	keys, err := bootstrap.acquireHostKeys(
		context.Background(),
		newConfirmationReader(strings.NewReader("")),
		"forge.example",
		"22",
	)
	if err != nil {
		t.Fatalf("acquireHostKeys: %v", err)
	}
	if len(keys) != 1 {
		t.Fatalf("got %d keys, want exactly 1 (only the matched key)", len(keys))
	}
	if ssh.FingerprintSHA256(keys[0]) != fingerprint {
		t.Fatalf("returned key fingerprint = %s, want %s", ssh.FingerprintSHA256(keys[0]), fingerprint)
	}
	if ssh.FingerprintSHA256(keys[0]) == ssh.FingerprintSHA256(attackerKey) {
		t.Fatal("attacker key was included in the returned set")
	}
}

// TestAcquireHostKeysFingerprintPathWorksWithSinglePinnedKeyType verifies that
// the connection flow works when the fingerprint path returns a single key
// type (the matched one). The known_hosts built from this single key must be
// a valid known_hosts document and the key must survive a round-trip through
// appendKnownHosts and knownHostKeys.
func TestAcquireHostKeysFingerprintPathWorksWithSinglePinnedKeyType(t *testing.T) {
	t.Parallel()
	legitPublic, _, err := ed25519.GenerateKey(cryptorand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	legitKey, err := ssh.NewPublicKey(legitPublic)
	if err != nil {
		t.Fatal(err)
	}
	attackerPublic, _, err := ed25519.GenerateKey(cryptorand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	attackerKey, err := ssh.NewPublicKey(attackerPublic)
	if err != nil {
		t.Fatal(err)
	}
	fingerprint := ssh.FingerprintSHA256(legitKey)
	bootstrap := UpstreamSSHBootstrap{
		Output:                  io.Discard,
		ExpectedHostFingerprint: fingerprint,
		ScanHostKeys: func(context.Context, string, string) ([]ssh.PublicKey, error) {
			return []ssh.PublicKey{attackerKey, legitKey}, nil
		},
	}

	keys, err := bootstrap.acquireHostKeys(
		context.Background(),
		newConfirmationReader(strings.NewReader("")),
		"forge.example",
		"22",
	)
	if err != nil {
		t.Fatalf("acquireHostKeys: %v", err)
	}

	// Build known_hosts from the returned keys and verify the round-trip.
	address := "forge.example:22"
	knownHosts := appendKnownHosts(nil, address, keys)
	roundTripped, err := knownHostKeys(knownHosts, address)
	if err != nil {
		t.Fatalf("knownHostKeys round-trip: %v", err)
	}
	if len(roundTripped) != 1 {
		t.Fatalf("round-tripped %d keys, want 1", len(roundTripped))
	}
	if ssh.FingerprintSHA256(roundTripped[0]) != fingerprint {
		t.Fatalf("round-tripped key fingerprint = %s, want %s",
			ssh.FingerprintSHA256(roundTripped[0]), fingerprint)
	}
}

func TestAcquireHostKeysExpectedFingerprintMismatchShowsBoth(t *testing.T) {
	t.Parallel()
	public, _, err := ed25519.GenerateKey(cryptorand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	key, err := ssh.NewPublicKey(public)
	if err != nil {
		t.Fatal(err)
	}
	scannedFingerprint := ssh.FingerprintSHA256(key)
	expectedFingerprint := "SHA256:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="
	bootstrap := UpstreamSSHBootstrap{
		Output:                  io.Discard,
		AutoYes:                 true,
		ExpectedHostFingerprint: expectedFingerprint,
		ScanHostKeys: func(context.Context, string, string) ([]ssh.PublicKey, error) {
			return []ssh.PublicKey{key}, nil
		},
	}

	_, err = bootstrap.acquireHostKeys(
		context.Background(),
		newConfirmationReader(strings.NewReader("")),
		"forge.example",
		"22",
	)
	if err == nil {
		t.Fatal("mismatched fingerprint was accepted")
	}
	if !strings.Contains(err.Error(), expectedFingerprint) || !strings.Contains(err.Error(), scannedFingerprint) {
		t.Fatalf("error must show both fingerprints, got: %v", err)
	}
}

// TestWaitForRegistrationClassifiesProbeErrors pins the wait-loop contract
// (#88/#89 hardening): the expected publickey rejection stays a silent dot,
// every other error — a knownhosts mismatch (active MITM indicator), a
// network fault, or a corrupt persisted identity — is surfaced as a warning
// line, and the timed-out summary carries the last probe error so the session
// itself holds the diagnosis.
func TestWaitForRegistrationClassifiesProbeErrors(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name        string
		probeErr    error
		wantWarning bool
	}{
		{
			name:        "publickey rejection is silent",
			probeErr:    errors.New("authenticate to SSH upstream: ssh: handshake failed: ssh: unable to authenticate, attempted methods [none publickey], no supported methods remain"),
			wantWarning: false,
		},
		{
			name:        "host-key mismatch is surfaced",
			probeErr:    errors.New("authenticate to SSH upstream: ssh: handshake failed: knownhosts: key mismatch"),
			wantWarning: true,
		},
		{
			name:        "connection refused is surfaced",
			probeErr:    errors.New("connect to SSH upstream: dial tcp 203.0.113.7:22: connect: connection refused"),
			wantWarning: true,
		},
		{
			name:        "corrupt persisted identity is surfaced",
			probeErr:    errors.New("parse SSH identity for authentication probe: ssh: no key found"),
			wantWarning: true,
		},
	} {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			var output bytes.Buffer
			bootstrap := UpstreamSSHBootstrap{
				Output: &output,
				Probe: func(context.Context, string, []byte, []byte) error {
					return test.probeErr
				},
				ProbeInterval: time.Millisecond,
				ProbeWait:     250 * time.Millisecond,
			}
			if accepted := bootstrap.waitForRegistration(context.Background(), "ssh://git@forge.example.com/acme", "forge.example.com", "22", nil, nil); accepted {
				t.Fatal("waitForRegistration unexpectedly reported acceptance")
			}
			text := output.String()
			if got := strings.Contains(text, "warning:"); got != test.wantWarning {
				t.Fatalf("warning surfaced = %v, want %v; output:\n%s", got, test.wantWarning, text)
			}
			if strings.Contains(text, "·") == test.wantWarning {
				t.Fatalf("silent dots must appear exactly when the error is the expected rejection; output:\n%s", text)
			}
			if !strings.Contains(text, "timed out") || !strings.Contains(text, "last probe error:") {
				t.Fatalf("timed-out summary must carry the last probe error; output:\n%s", text)
			}
		})
	}
}
