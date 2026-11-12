package app

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	cryptorand "crypto/rand"
	"crypto/rsa"
	"encoding/pem"
	"errors"
	"io"
	"net"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"

	"github.com/oberthci/oberth/internal/testtimeout"
)

// multiKeyForge is a stock-OpenSSH-like forge: it offers RSA, ECDSA and
// Ed25519 host keys and accepts one client key, opening no channels.
type multiKeyForge struct {
	address string
	keys    map[string]ssh.PublicKey // by key type
}

func startMultiKeyForge(t *testing.T, client ssh.PublicKey) multiKeyForge {
	t.Helper()
	_, ed, err := ed25519.GenerateKey(cryptorand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	ec, err := ecdsa.GenerateKey(elliptic.P256(), cryptorand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	rsaKey, err := rsa.GenerateKey(cryptorand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	config := &ssh.ServerConfig{PublicKeyCallback: func(_ ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
		if !bytes.Equal(key.Marshal(), client.Marshal()) {
			return nil, errors.New("unknown key")
		}
		return nil, nil
	}}
	forge := multiKeyForge{keys: map[string]ssh.PublicKey{}}
	for _, key := range []any{rsaKey, ec, ed} {
		signer, err := ssh.NewSignerFromKey(key)
		if err != nil {
			t.Fatal(err)
		}
		config.AddHostKey(signer)
		forge.keys[signer.PublicKey().Type()] = signer.PublicKey()
	}
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	forge.address = listener.Addr().String()
	var workers sync.WaitGroup
	workers.Go(func() {
		for {
			connection, err := listener.Accept()
			if err != nil {
				return
			}
			workers.Go(func() {
				defer func() { _ = connection.Close() }()
				_ = connection.SetDeadline(time.Now().Add(testtimeout.Start(10 * time.Second)))
				server, channels, requests, err := ssh.NewServerConn(connection, config)
				if err != nil {
					return
				}
				defer func() { _ = server.Close() }()
				go ssh.DiscardRequests(requests)
				for channel := range channels {
					_ = channel.Reject(ssh.Prohibited, "no channels")
				}
			})
		}
	})
	t.Cleanup(func() { _ = listener.Close(); workers.Wait() })
	return forge
}

func testClientIdentity(t *testing.T) (ssh.PublicKey, []byte) {
	t.Helper()
	public, private, err := ed25519.GenerateKey(cryptorand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	key, err := ssh.NewPublicKey(public)
	if err != nil {
		t.Fatal(err)
	}
	block, err := ssh.MarshalPrivateKey(private, "test")
	if err != nil {
		t.Fatal(err)
	}
	return key, pem.EncodeToMemory(block)
}

// The probe must negotiate the pinned key type. Go prefers ECDSA over
// Ed25519, so before the fix a forge with both keys and an Ed25519-only pin
// (what --expected-host-fingerprint pins) always failed as a key mismatch.
func TestProbeSSHAuthenticationNegotiatesOnlyPinnedKeyTypes(t *testing.T) {
	t.Parallel()
	client, private := testClientIdentity(t)
	forge := startMultiKeyForge(t, client)
	for _, keyType := range []string{ssh.KeyAlgoED25519, ssh.KeyAlgoECDSA256, ssh.KeyAlgoRSA} {
		t.Run(keyType, func(t *testing.T) {
			t.Parallel()
			pins := []byte(knownhosts.Line([]string{forge.address}, forge.keys[keyType]) + "\n")
			ctx, cancel := context.WithTimeout(context.Background(), testtimeout.Start(10*time.Second))
			defer cancel()
			if err := ProbeSSHAuthentication(ctx, "ssh://git@"+forge.address+"/acme", private, pins); err != nil {
				t.Fatalf("probe with a %s-only pin: %v", keyType, err)
			}
		})
	}
}

func TestProbeSSHAuthenticationNamesRepinOnKeyMismatch(t *testing.T) {
	t.Parallel()
	client, private := testClientIdentity(t)
	forge := startMultiKeyForge(t, client)
	other, _ := testClientIdentity(t)
	pins := []byte(knownhosts.Line([]string{forge.address}, other) + "\n")
	ctx, cancel := context.WithTimeout(context.Background(), testtimeout.Start(10*time.Second))
	defer cancel()
	err := ProbeSSHAuthentication(ctx, "ssh://git@"+forge.address+"/acme", private, pins)
	if err == nil || !strings.Contains(err.Error(), "key mismatch") || !strings.Contains(err.Error(), "--repin") {
		t.Fatalf("mismatch error = %v, want a key mismatch naming --repin", err)
	}
}

type memoryIdentityStore struct {
	mu    sync.Mutex
	data  map[string]map[string][]byte
	saves []string
}

func (s *memoryIdentityStore) Load(_ context.Context, name string) (map[string][]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	result := map[string][]byte{}
	for key, value := range s.data[name] {
		result[key] = bytes.Clone(value)
	}
	return result, nil
}

func (s *memoryIdentityStore) Save(_ context.Context, name string, values map[string][]byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.data == nil {
		s.data = map[string]map[string][]byte{}
	}
	if s.data[name] == nil {
		s.data[name] = map[string][]byte{}
	}
	for key, value := range values {
		s.data[name][key] = bytes.Clone(value)
	}
	s.saves = append(s.saves, name)
	return nil
}

func repinBootstrap(t *testing.T, store *memoryIdentityStore, fingerprint string) UpstreamSSHBootstrap {
	t.Helper()
	directory := t.TempDir()
	return UpstreamSSHBootstrap{
		Store: store, Input: strings.NewReader(""), Output: io.Discard,
		ScanHostKeys: ScanSSHHostKeys, Probe: ProbeSSHAuthentication,
		MutationGate: func(context.Context, string) error { return nil },
		Namespace:    "oberth", PrivateKeySecret: "oberth-upstream-key", KnownHostsSecret: "oberth-known-hosts",
		PrivateKeyPath: filepath.Join(directory, "upstream", "id_ed25519"), KnownHostsPath: filepath.Join(directory, "hosts", "known_hosts"),
		PrivateKeyDataKey: "id_ed25519", PublicKeyDataKey: "id_ed25519.pub", KnownHostsDataKey: "known_hosts",
		AutoYes: true, ExpectedHostFingerprint: fingerprint,
		ProbeWait: 20 * time.Millisecond, ProbeInterval: time.Millisecond,
	}
}

// End to end on a multi-key forge: --expected-host-fingerprint pins only the
// matching Ed25519 key and the authentication probe then succeeds. --repin
// replaces this endpoint's stale pin through the audited store write while
// keeping another endpoint's pin.
func TestUpstreamBootstrapExpectedFingerprintAndRepinOnMultiKeyForge(t *testing.T) {
	t.Parallel()
	client, private := testClientIdentity(t)
	forge := startMultiKeyForge(t, client)
	edFingerprint := ssh.FingerprintSHA256(forge.keys[ssh.KeyAlgoED25519])
	otherKey, _ := testClientIdentity(t)
	otherLine := knownhosts.Line([]string{"other.example"}, otherKey) + "\n"
	stale, _ := testClientIdentity(t)
	store := &memoryIdentityStore{data: map[string]map[string][]byte{
		"oberth-upstream-key": {"id_ed25519": private, "id_ed25519.pub": ssh.MarshalAuthorizedKey(client)},
		"oberth-known-hosts":  {"known_hosts": []byte(otherLine + knownhosts.Line([]string{forge.address}, stale) + "\n")},
	}}
	baseURL := "ssh://git@" + forge.address + "/acme"
	ctx, cancel := context.WithTimeout(context.Background(), testtimeout.Start(30*time.Second))
	defer cancel()

	// A stale pin is reused without --repin, so the probe refuses it.
	if _, err := repinBootstrap(t, store, edFingerprint).Ensure(ctx, baseURL); err == nil || !strings.Contains(err.Error(), "--repin") {
		t.Fatalf("stale pin error = %v, want a mismatch naming --repin", err)
	}

	bootstrap := repinBootstrap(t, store, edFingerprint)
	bootstrap.Repin = true
	if _, err := bootstrap.Ensure(ctx, baseURL); err != nil {
		t.Fatalf("repin: %v", err)
	}
	pins := string(store.data["oberth-known-hosts"]["known_hosts"])
	if !strings.Contains(pins, otherLine) {
		t.Fatalf("repin dropped another endpoint's pin: %q", pins)
	}
	keys, err := knownHostKeys([]byte(pins), forge.address)
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 1 || ssh.FingerprintSHA256(keys[0]) != edFingerprint {
		t.Fatalf("pins for the forge = %d keys, want only the expected Ed25519 key", len(keys))
	}

	// The re-pinned bundle now authenticates without another scan.
	if _, err := repinBootstrap(t, store, "").Ensure(ctx, baseURL); err != nil {
		t.Fatalf("rerun after repin: %v", err)
	}

	// A wrong expected fingerprint never replaces the pins.
	before := bytes.Clone(store.data["oberth-known-hosts"]["known_hosts"])
	bootstrap = repinBootstrap(t, store, ssh.FingerprintSHA256(stale))
	bootstrap.Repin = true
	if _, err := bootstrap.Ensure(ctx, baseURL); err == nil || !strings.Contains(err.Error(), "fingerprint mismatch") {
		t.Fatalf("repin with a wrong fingerprint = %v", err)
	}
	if !bytes.Equal(before, store.data["oberth-known-hosts"]["known_hosts"]) {
		t.Fatal("a refused repin changed the pins")
	}
}

func TestWithoutKnownHostsAddressRefusesForeignEntries(t *testing.T) {
	t.Parallel()
	key, _ := testClientIdentity(t)
	address := "forge.example:2222"
	own := knownhosts.Line([]string{address}, key) + "\n"
	kept := "# comment\n" + knownhosts.Line([]string{"other.example"}, key) + "\n"
	result, err := withoutKnownHostsAddress([]byte(kept+own), address)
	if err != nil || string(result) != kept {
		t.Fatalf("strip = %q, %v; want %q", result, err, kept)
	}
	hashed := knownhosts.HashHostname(knownhosts.Normalize(address)) + " " + strings.TrimSpace(string(ssh.MarshalAuthorizedKey(key))) + "\n"
	multi := knownhosts.Line([]string{address, "alias.example"}, key) + "\n"
	for name, body := range map[string]string{"hashed": hashed, "multi-host": multi} {
		if _, err := withoutKnownHostsAddress([]byte(body), address); err == nil {
			t.Errorf("%s entry for the endpoint was not refused", name)
		}
	}
}

func TestPinnedHostKeyAlgorithmsNeverOffersSHA1RSA(t *testing.T) {
	t.Parallel()
	rsaKey, err := rsa.GenerateKey(cryptorand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(rsaKey)
	if err != nil {
		t.Fatal(err)
	}
	address := "forge.example:22"
	algorithms, err := pinnedHostKeyAlgorithms([]byte(knownhosts.Line([]string{address}, signer.PublicKey())+"\n"), address)
	if err != nil {
		t.Fatal(err)
	}
	for _, algorithm := range algorithms {
		if algorithm == ssh.KeyAlgoRSA {
			t.Fatalf("algorithms = %v offer SHA-1 ssh-rsa", algorithms)
		}
	}
	if len(algorithms) == 0 {
		t.Fatal("no algorithms for an RSA pin")
	}
	if _, err := pinnedHostKeyAlgorithms([]byte(knownhosts.Line([]string{"other.example"}, signer.PublicKey())+"\n"), address); err == nil {
		t.Fatal("algorithms for an endpoint without pins")
	}
}

// An interactive re-pin shows the pins it replaces beside the scanned keys,
// so the operator can tell a rotation from a swap, and its store write is
// audited under its own operation, apart from a first pin.
func TestUpstreamBootstrapRepinShowsReplacedPinsAndAuditsDistinctly(t *testing.T) {
	t.Parallel()
	client, private := testClientIdentity(t)
	forge := startMultiKeyForge(t, client)
	stale, _ := testClientIdentity(t)
	kept := forge.keys[ssh.KeyAlgoED25519]
	store := &memoryIdentityStore{data: map[string]map[string][]byte{
		"oberth-upstream-key": {"id_ed25519": private, "id_ed25519.pub": ssh.MarshalAuthorizedKey(client)},
		"oberth-known-hosts": {"known_hosts": []byte(knownhosts.Line([]string{forge.address}, stale) + "\n" +
			knownhosts.Line([]string{forge.address}, kept) + "\n")},
	}}
	ctx, cancel := context.WithTimeout(context.Background(), testtimeout.Start(30*time.Second))
	defer cancel()
	var output bytes.Buffer
	var operations []string
	bootstrap := repinBootstrap(t, store, "")
	bootstrap.Repin, bootstrap.AutoYes = true, false
	bootstrap.Input, bootstrap.Output = strings.NewReader("yes\n"), &output
	bootstrap.MutationGate = func(_ context.Context, operation string) error {
		operations = append(operations, operation)
		return nil
	}
	if _, err := bootstrap.Ensure(ctx, "ssh://git@"+forge.address+"/acme"); err != nil {
		t.Fatalf("interactive repin: %v\n%s", err, output.String())
	}
	text := output.String()
	pinned, offered, found := strings.Cut(text, "fingerprints offered by")
	if !found || !strings.Contains(pinned, "currently pinned") {
		t.Fatalf("prompt does not list the replaced pins before the offered keys: %q", text)
	}
	for _, key := range []ssh.PublicKey{stale, kept} {
		if !strings.Contains(pinned, ssh.FingerprintSHA256(key)) {
			t.Errorf("replaced pin %s is not shown: %q", ssh.FingerprintSHA256(key), pinned)
		}
	}
	if !strings.Contains(offered, ssh.FingerprintSHA256(kept)+" (already pinned)") ||
		!strings.Contains(offered, ssh.FingerprintSHA256(forge.keys[ssh.KeyAlgoECDSA256])+" (new)") ||
		strings.Contains(offered, ssh.FingerprintSHA256(stale)) {
		t.Fatalf("offered keys are not marked against the replaced pins: %q", offered)
	}
	if len(operations) != 1 || operations[0] != "upstream.identity.repin.oberth-known-hosts" {
		t.Fatalf("audit operations = %v, want one distinct re-pin operation", operations)
	}

	// A first pin keeps the plain identity operation and shows no old pins.
	fresh := &memoryIdentityStore{data: map[string]map[string][]byte{
		"oberth-upstream-key": {"id_ed25519": private, "id_ed25519.pub": ssh.MarshalAuthorizedKey(client)},
	}}
	output.Reset()
	operations = nil
	first := repinBootstrap(t, fresh, "")
	first.AutoYes = false
	first.Input, first.Output = strings.NewReader("yes\n"), &output
	first.MutationGate = bootstrap.MutationGate
	if _, err := first.Ensure(ctx, "ssh://git@"+forge.address+"/acme"); err != nil {
		t.Fatalf("first pin: %v", err)
	}
	if strings.Contains(output.String(), "currently pinned") || strings.Contains(output.String(), "(new)") {
		t.Fatalf("a first pin shows re-pin annotations: %q", output.String())
	}
	if len(operations) != 1 || operations[0] != "upstream.identity.oberth-known-hosts" {
		t.Fatalf("first-pin audit operations = %v", operations)
	}
}
