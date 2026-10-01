package app

import (
	"bytes"
	"context"
	"crypto/ed25519"
	cryptorand "crypto/rand"
	"encoding/pem"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"

	"golang.org/x/crypto/ssh"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
)

// ---------------------------------------------------------------------------
// SSHPublicKeyFingerprint (git_env.go, 0% coverage)
// ---------------------------------------------------------------------------

func TestSSHPublicKeyFingerprint_Ed25519(t *testing.T) {
	t.Parallel()
	_, privateKey, err := ed25519.GenerateKey(cryptorand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	block, err := ssh.MarshalPrivateKey(privateKey, "test")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "id_ed25519")
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(block), 0o600); err != nil {
		t.Fatal(err)
	}

	fingerprint, err := SSHPublicKeyFingerprint(keyPath)
	if err != nil {
		t.Fatalf("SSHPublicKeyFingerprint: %v", err)
	}
	if !strings.HasPrefix(fingerprint, "SHA256:") {
		t.Fatalf("fingerprint = %q, want SHA256: prefix", fingerprint)
	}
}

func TestSSHPublicKeyFingerprint_WorldReadableKey(t *testing.T) {
	t.Parallel()
	_, privateKey, err := ed25519.GenerateKey(cryptorand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	block, err := ssh.MarshalPrivateKey(privateKey, "test")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "id_ed25519")
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(block), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err = SSHPublicKeyFingerprint(keyPath)
	if err == nil {
		t.Fatal("world-readable private key passed validation")
	}
}

func TestSSHPublicKeyFingerprint_MissingFile(t *testing.T) {
	t.Parallel()
	_, err := SSHPublicKeyFingerprint("/nonexistent/path/id_ed25519")
	if err == nil {
		t.Fatal("missing file passed validation")
	}
}

func TestSSHPublicKeyFingerprint_InvalidPath(t *testing.T) {
	t.Parallel()
	_, err := SSHPublicKeyFingerprint("relative/path")
	if err == nil {
		t.Fatal("relative path passed validation")
	}
}

// ---------------------------------------------------------------------------
// UpstreamKind (upstream.go, 62.5% coverage)
// ---------------------------------------------------------------------------

func TestUpstreamKind_Local(t *testing.T) {
	t.Parallel()
	kind, err := UpstreamKind(filepath.Join(t.TempDir(), "forge"))
	if err != nil || kind != "local" {
		t.Fatalf("kind = %q, err = %v", kind, err)
	}
}

func TestUpstreamKind_HTTPS(t *testing.T) {
	t.Parallel()
	kind, err := UpstreamKind("https://forge.example.net/acme")
	if err != nil || kind != "https" {
		t.Fatalf("kind = %q, err = %v", kind, err)
	}
}

func TestUpstreamKind_InvalidBase(t *testing.T) {
	t.Parallel()
	_, err := UpstreamKind("ssh://git@forge.example/org/repo.git")
	if err == nil {
		t.Fatal("expected error for .git base URL")
	}
}

// ---------------------------------------------------------------------------
// Upstreams.timeout (upstream.go, 66.7% coverage)
// ---------------------------------------------------------------------------

func TestUpstreamsTimeout_CustomValue(t *testing.T) {
	t.Parallel()
	upstreams := Upstreams{Timeout: 30 * time.Second}
	if upstreams.timeout() != 30*time.Second {
		t.Fatalf("timeout = %v, want 30s", upstreams.timeout())
	}
}

func TestUpstreamsTimeout_Default(t *testing.T) {
	t.Parallel()
	upstreams := Upstreams{}
	if upstreams.timeout() != defaultCatalogTimeout {
		t.Fatalf("timeout = %v, want %v", upstreams.timeout(), defaultCatalogTimeout)
	}
}

// ---------------------------------------------------------------------------
// Health edge cases (health.go)
// ---------------------------------------------------------------------------

func TestHealthReady_NilDependencies(t *testing.T) {
	t.Parallel()
	health := Health{}
	err := health.Ready(context.Background())
	if err == nil || !strings.Contains(err.Error(), "dependencies are unavailable") {
		t.Fatalf("expected dependencies-unavailable error, got: %v", err)
	}
}

func TestHealthStatus_NilStore(t *testing.T) {
	t.Parallel()
	health := Health{}
	value, err := health.Status(context.Background())
	if err != nil {
		t.Fatalf("Status with nil store: %v", err)
	}
	status := value.(HealthStatus)
	if status.Database != "unavailable" {
		t.Fatalf("database = %q, want unavailable", status.Database)
	}
}

func TestHealthStatus_AuditChainError(t *testing.T) {
	t.Parallel()
	health := Health{
		Store:      fakeHealthStore{upstreams: []model.Upstream{{ID: 1}}},
		Configured: func(context.Context) error { return nil },
		Cluster:    func(context.Context) error { return nil },
		Audit:      func(context.Context) error { return nil },
		VCS:        func(context.Context, model.Upstream) error { return nil },
		AuditChain: func(context.Context) (AuditChainStatus, error) {
			return AuditChainStatus{}, errors.New("chain broken")
		},
	}
	value, err := health.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	status := value.(HealthStatus)
	// AuditChain error means the chain detail is nil.
	if status.AuditChain != nil {
		t.Fatal("expected nil AuditChain when func errors")
	}
}

func TestHealthStatus_IdentityError(t *testing.T) {
	t.Parallel()
	health := Health{
		Store:      fakeHealthStore{upstreams: []model.Upstream{{ID: 1}}},
		Configured: func(context.Context) error { return nil },
		Cluster:    func(context.Context) error { return nil },
		Audit:      func(context.Context) error { return nil },
		VCS:        func(context.Context, model.Upstream) error { return nil },
		Identity:   func(context.Context) (string, error) { return "", errors.New("key missing") },
	}
	value, err := health.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	status := value.(HealthStatus)
	if status.SSHIdentity != "" {
		t.Fatalf("SSHIdentity = %q, want empty on error", status.SSHIdentity)
	}
}

func TestHealthStatus_ZeroUpstreams(t *testing.T) {
	t.Parallel()
	health := Health{
		Store:   fakeHealthStore{},
		Cluster: func(context.Context) error { return nil },
		Audit:   func(context.Context) error { return nil },
		VCS:     func(context.Context, model.Upstream) error { return nil },
	}
	value, err := health.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	status := value.(HealthStatus)
	// VCS should stay "unavailable" when no upstreams exist.
	if status.VCS != "unavailable" {
		t.Fatalf("VCS = %q, want unavailable with zero upstreams", status.VCS)
	}
}

// ---------------------------------------------------------------------------
// boundedDetail (health.go, 75% coverage)
// ---------------------------------------------------------------------------

func TestBoundedDetail_LongString(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("x", 500)
	result := boundedDetail(long)
	if len(result) > 305 {
		t.Fatalf("boundedDetail length = %d, want <= 305", len(result))
	}
	if !strings.HasSuffix(result, "…") {
		t.Fatal("long string should end with ellipsis")
	}
}

func TestBoundedDetail_Whitespace(t *testing.T) {
	t.Parallel()
	result := boundedDetail("  hello   world  \n\t ok  ")
	if result != "hello world ok" {
		t.Fatalf("boundedDetail = %q", result)
	}
}

// ---------------------------------------------------------------------------
// upstream_bootstrap.go pure functions (many at 0%)
// ---------------------------------------------------------------------------

func TestUpstreamKeyFieldManager(t *testing.T) {
	t.Parallel()
	manager := UpstreamKeyFieldManager("codeberg")
	want := "oberth-upstream-bootstrap-codeberg"
	if manager != want {
		t.Fatalf("field manager = %q, want %q", manager, want)
	}
}

func TestTerseProbeError(t *testing.T) {
	t.Parallel()
	short := terseProbeError(errors.New("connection refused"))
	if short != "connection refused" {
		t.Fatalf("terseProbeError = %q", short)
	}

	long := terseProbeError(errors.New(strings.Repeat("x", 200)))
	if len(long) > 165 {
		t.Fatalf("terseProbeError length = %d, want <= 165", len(long))
	}
}

func TestTerseProbeError_CompressesWhitespace(t *testing.T) {
	t.Parallel()
	result := terseProbeError(errors.New("  error  with\n\tmultiple   spaces  "))
	if strings.Contains(result, "  ") || strings.Contains(result, "\n") {
		t.Fatalf("terseProbeError = %q, want compressed whitespace", result)
	}
}

func TestGeneratePrivateIdentity(t *testing.T) {
	t.Parallel()
	identity, err := generatePrivateIdentity(nil)
	if err != nil {
		t.Fatalf("generatePrivateIdentity: %v", err)
	}
	if len(identity.privateKey) == 0 {
		t.Fatal("private key is empty")
	}
	if len(identity.publicKey) == 0 {
		t.Fatal("public key is empty")
	}
	if !strings.HasPrefix(identity.fingerprint, "SHA256:") {
		t.Fatalf("fingerprint = %q, want SHA256: prefix", identity.fingerprint)
	}
	if !strings.Contains(string(identity.publicKey), "ssh-ed25519") {
		t.Fatalf("public key does not contain ssh-ed25519: %s", identity.publicKey)
	}
}

func TestParsePrivateIdentity_Empty(t *testing.T) {
	t.Parallel()
	_, err := parsePrivateIdentity(nil)
	if err == nil {
		t.Fatal("expected error for empty input")
	}
}

func TestParsePrivateIdentity_ValidKey(t *testing.T) {
	t.Parallel()
	_, privateKey, err := ed25519.GenerateKey(cryptorand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	block, err := ssh.MarshalPrivateKey(privateKey, "test")
	if err != nil {
		t.Fatal(err)
	}
	body := pem.EncodeToMemory(block)
	identity, err := parsePrivateIdentity(body)
	if err != nil {
		t.Fatalf("parsePrivateIdentity: %v", err)
	}
	if !strings.HasPrefix(identity.fingerprint, "SHA256:") {
		t.Fatalf("fingerprint = %q", identity.fingerprint)
	}
}

func TestParsePrivateIdentity_InvalidKey(t *testing.T) {
	t.Parallel()
	_, err := parsePrivateIdentity([]byte("not a key"))
	if err == nil {
		t.Fatal("expected error for invalid key")
	}
}

func TestSameAuthorizedKey_Matching(t *testing.T) {
	t.Parallel()
	public, _, err := ed25519.GenerateKey(cryptorand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	sshKey, err := ssh.NewPublicKey(public)
	if err != nil {
		t.Fatal(err)
	}
	marshaled := ssh.MarshalAuthorizedKey(sshKey)
	same, err := sameAuthorizedKey(marshaled, marshaled)
	if err != nil {
		t.Fatalf("sameAuthorizedKey: %v", err)
	}
	if !same {
		t.Fatal("same key reported as different")
	}
}

func TestSameAuthorizedKey_Different(t *testing.T) {
	t.Parallel()
	public1, _, _ := ed25519.GenerateKey(cryptorand.Reader)
	public2, _, _ := ed25519.GenerateKey(cryptorand.Reader)
	key1, _ := ssh.NewPublicKey(public1)
	key2, _ := ssh.NewPublicKey(public2)
	same, err := sameAuthorizedKey(ssh.MarshalAuthorizedKey(key1), ssh.MarshalAuthorizedKey(key2))
	if err != nil {
		t.Fatalf("sameAuthorizedKey: %v", err)
	}
	if same {
		t.Fatal("different keys reported as same")
	}
}

func TestSameAuthorizedKey_InvalidLeft(t *testing.T) {
	t.Parallel()
	_, err := sameAuthorizedKey([]byte("bad"), []byte("bad"))
	if err == nil {
		t.Fatal("expected error for invalid key")
	}
}

func TestPublicIdentity(t *testing.T) {
	t.Parallel()
	identity, err := generatePrivateIdentity(nil)
	if err != nil {
		t.Fatal(err)
	}
	pub := identity.publicIdentity(true)
	if pub.Fingerprint != identity.fingerprint {
		t.Fatalf("fingerprint mismatch: %q vs %q", pub.Fingerprint, identity.fingerprint)
	}
	if !pub.Generated {
		t.Fatal("expected Generated=true")
	}
	if !bytes.Equal(pub.PublicKey, identity.publicKey) {
		t.Fatal("public key mismatch")
	}

	pub2 := identity.publicIdentity(false)
	if pub2.Generated {
		t.Fatal("expected Generated=false")
	}
}

func TestParseScannedHostKeys_Empty(t *testing.T) {
	t.Parallel()
	keys, err := parseScannedHostKeys(nil)
	if err != nil {
		t.Fatalf("parseScannedHostKeys(nil): %v", err)
	}
	if len(keys) != 0 {
		t.Fatalf("expected zero keys, got %d", len(keys))
	}
}

func TestParseScannedHostKeys_ValidOutput(t *testing.T) {
	t.Parallel()
	public, _, err := ed25519.GenerateKey(cryptorand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	sshKey, err := ssh.NewPublicKey(public)
	if err != nil {
		t.Fatal(err)
	}
	// ssh-keyscan output format: hostname key-type base64-key
	line := "forge.example " + string(bytes.TrimSpace(ssh.MarshalAuthorizedKey(sshKey))) + "\n"
	keys, err := parseScannedHostKeys([]byte(line))
	if err != nil {
		t.Fatalf("parseScannedHostKeys: %v", err)
	}
	if len(keys) != 1 {
		t.Fatalf("expected 1 key, got %d", len(keys))
	}
}

func TestCappedBuffer_Write(t *testing.T) {
	t.Parallel()
	buf := &cappedBuffer{limit: 10}
	n, err := buf.Write([]byte("hello"))
	if err != nil || n != 5 {
		t.Fatalf("Write = %d, %v", n, err)
	}
	if buf.overflow {
		t.Fatal("overflow should be false after small write")
	}

	// Write more than remaining capacity.
	n, err = buf.Write([]byte("world!!!!"))
	if err != nil {
		t.Fatalf("Write = %d, %v", n, err)
	}
	if n != 9 {
		t.Fatalf("Write should report original length %d, got %d", 9, n)
	}
	if !buf.overflow {
		t.Fatal("overflow should be true after exceeding limit")
	}
	if buf.Len() != 10 {
		t.Fatalf("buffer length = %d, want 10", buf.Len())
	}
}

func TestCappedBuffer_WriteAtLimit(t *testing.T) {
	t.Parallel()
	buf := &cappedBuffer{limit: 5}
	buf.Write([]byte("12345"))
	// Buffer is exactly full; next write should overflow.
	n, err := buf.Write([]byte("X"))
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if n != 1 {
		t.Fatalf("Write = %d, want 1", n)
	}
	if !buf.overflow {
		t.Fatal("overflow should be true at limit")
	}
	if buf.String() != "12345" {
		t.Fatalf("buffer = %q, want 12345", buf.String())
	}
}

func TestStringAddress_NetworkAndString(t *testing.T) {
	t.Parallel()
	addr := stringAddress("127.0.0.1:22")
	if addr.Network() != "tcp" {
		t.Fatalf("Network = %q", addr.Network())
	}
	if addr.String() != "127.0.0.1:22" {
		t.Fatalf("String = %q", addr.String())
	}
}

// ---------------------------------------------------------------------------
// upstream_bootstrap.go validate
// ---------------------------------------------------------------------------

func TestUpstreamSSHBootstrap_Validate_Minimal(t *testing.T) {
	t.Parallel()
	// Missing required fields.
	bootstrap := UpstreamSSHBootstrap{}
	err := bootstrap.validate()
	if err == nil {
		t.Fatal("expected validation error for empty bootstrap")
	}
	if !strings.Contains(err.Error(), "required") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestUpstreamSSHBootstrap_Validate_SameSecrets(t *testing.T) {
	t.Parallel()
	clientset := fake.NewSimpleClientset()
	bootstrap := UpstreamSSHBootstrap{
		Input:  strings.NewReader(""),
		Output: &bytes.Buffer{},
		KubernetesClient: func() (kubernetes.Interface, error) {
			return clientset, nil
		},
		ScanHostKeys: func(context.Context, string, string) ([]ssh.PublicKey, error) { return nil, nil },
		Probe: func(context.Context, string, []byte, []byte) error {
			return nil
		},
		PrivateKeySecret:  "same-secret",
		KnownHostsSecret:  "same-secret",
		Namespace:         "oberth",
		PrivateKeyDataKey: "key",
		PublicKeyDataKey:  "key.pub",
		KnownHostsDataKey: "known_hosts",
		PrivateKeyPath:    "/etc/oberth/key",
		KnownHostsPath:    "/etc/oberth/known_hosts",
	}
	err := bootstrap.validate()
	if err == nil || !strings.Contains(err.Error(), "must be distinct") {
		t.Fatalf("expected distinct-secrets error, got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// selectKnownHosts
// ---------------------------------------------------------------------------

func TestSelectKnownHosts_ProjectedValid(t *testing.T) {
	t.Parallel()
	public, _, err := ed25519.GenerateKey(cryptorand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	key, err := ssh.NewPublicKey(public)
	if err != nil {
		t.Fatal(err)
	}
	projected := knownHostsMaterial{
		body: []byte("forge.example:22 " + string(bytes.TrimSpace(ssh.MarshalAuthorizedKey(key))) + "\n"),
		keys: []ssh.PublicKey{key},
	}
	body, keys, err := selectKnownHosts(projected, nil, nil, "forge.example:22")
	if err != nil {
		t.Fatalf("selectKnownHosts: %v", err)
	}
	if len(keys) != 1 {
		t.Fatalf("keys = %d, want 1", len(keys))
	}
	if len(body) == 0 {
		t.Fatal("body is empty")
	}
}

func TestSelectKnownHosts_ProjectedError_Passthrough(t *testing.T) {
	t.Parallel()
	projErr := errors.New("permission denied")
	_, _, err := selectKnownHosts(knownHostsMaterial{}, projErr, nil, "x:22")
	if err == nil || !strings.Contains(err.Error(), "invalid") {
		t.Fatalf("expected invalid-known-hosts error, got: %v", err)
	}
}

func TestSelectKnownHosts_NoProjected_NoPersisted(t *testing.T) {
	t.Parallel()
	body, keys, err := selectKnownHosts(knownHostsMaterial{}, os.ErrNotExist, nil, "x:22")
	if err != nil {
		t.Fatalf("selectKnownHosts: %v", err)
	}
	if len(body) != 0 || len(keys) != 0 {
		t.Fatalf("expected empty results, got body=%d keys=%d", len(body), len(keys))
	}
}

// ---------------------------------------------------------------------------
// verifySecretData
// ---------------------------------------------------------------------------

func TestVerifySecretData_Match(t *testing.T) {
	t.Parallel()
	clientset := fake.NewClientset()
	bootstrap := UpstreamSSHBootstrap{
		Namespace:    "oberth",
		MutationGate: func(context.Context, string) error { return nil },
	}
	// Use the real Argo test path: NewClientset handles server-side apply.
	if err := bootstrap.applySecret(context.Background(), clientset, "test-secret", map[string][]byte{
		"key": []byte("value"),
	}, ""); err != nil {
		t.Fatal(err)
	}

	err := bootstrap.verifySecretData(context.Background(), clientset, "test-secret", map[string][]byte{
		"key": []byte("value"),
	})
	if err != nil {
		t.Fatalf("verifySecretData: %v", err)
	}
}

func TestVerifySecretData_MissingKey(t *testing.T) {
	t.Parallel()
	clientset := fake.NewClientset()
	bootstrap := UpstreamSSHBootstrap{
		Namespace:    "oberth",
		MutationGate: func(context.Context, string) error { return nil },
	}
	if err := bootstrap.applySecret(context.Background(), clientset, "test-secret-mismatch", map[string][]byte{
		"key": []byte("value"),
	}, ""); err != nil {
		t.Fatal(err)
	}

	err := bootstrap.verifySecretData(context.Background(), clientset, "test-secret-mismatch", map[string][]byte{
		"key":   []byte("value"),
		"other": []byte("missing"),
	})
	if err == nil || !strings.Contains(err.Error(), "did not retain data key") {
		t.Fatalf("expected mismatch error, got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// sshEndpoint
// ---------------------------------------------------------------------------

func TestSSHEndpoint_Valid(t *testing.T) {
	t.Parallel()
	host, port, address, err := sshEndpoint("ssh://git@forge.example:2222/acme")
	if err != nil {
		t.Fatalf("sshEndpoint: %v", err)
	}
	if host != "forge.example" || port != "2222" || address != "forge.example:2222" {
		t.Fatalf("host=%q port=%q address=%q", host, port, address)
	}
}

func TestSSHEndpoint_DefaultPort(t *testing.T) {
	t.Parallel()
	host, port, address, err := sshEndpoint("ssh://git@forge.example/acme")
	if err != nil {
		t.Fatalf("sshEndpoint: %v", err)
	}
	if host != "forge.example" || port != "22" || address != "forge.example:22" {
		t.Fatalf("host=%q port=%q address=%q", host, port, address)
	}
}

func TestSSHEndpoint_NotSSH(t *testing.T) {
	t.Parallel()
	_, _, _, err := sshEndpoint("https://forge.example/acme")
	if err == nil || !strings.Contains(err.Error(), "ssh://") {
		t.Fatalf("expected SSH-required error, got: %v", err)
	}
}

func TestSSHEndpoint_InvalidPort(t *testing.T) {
	t.Parallel()
	_, _, _, err := sshEndpoint("ssh://git@forge.example:99999/acme")
	if err == nil || !strings.Contains(err.Error(), "port") {
		t.Fatalf("expected port error, got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// printGeneratedIdentity / printPersistedIdentity
// ---------------------------------------------------------------------------

func TestPrintGeneratedIdentity(t *testing.T) {
	t.Parallel()
	identity, err := generatePrivateIdentity(nil)
	if err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	bootstrap := UpstreamSSHBootstrap{Output: &output}
	if err := bootstrap.printGeneratedIdentity(identity, "forge.example", "22"); err != nil {
		t.Fatalf("printGeneratedIdentity: %v", err)
	}
	if !strings.Contains(output.String(), "deploy key") {
		t.Fatalf("output = %q, want deploy key instructions", output.String())
	}
	if !strings.Contains(output.String(), string(identity.publicKey)) {
		t.Fatal("output does not contain the public key")
	}
}

func TestPrintPersistedIdentity(t *testing.T) {
	t.Parallel()
	identity, err := generatePrivateIdentity(nil)
	if err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	bootstrap := UpstreamSSHBootstrap{Output: &output}
	if err := bootstrap.printPersistedIdentity(identity, "forge.example", "22"); err != nil {
		t.Fatalf("printPersistedIdentity: %v", err)
	}
	if !strings.Contains(output.String(), "Persisted") {
		t.Fatalf("output = %q, want Persisted in output", output.String())
	}
	if !strings.Contains(output.String(), identity.fingerprint) {
		t.Fatal("output does not contain the fingerprint")
	}
}

// ---------------------------------------------------------------------------
// readBoundedFileForBootstrap
// ---------------------------------------------------------------------------

func TestReadBoundedFileForBootstrap_FromReader(t *testing.T) {
	t.Parallel()
	body, err := readBoundedFileForBootstrap("test", "", strings.NewReader("hello"))
	if err != nil {
		t.Fatalf("readBoundedFileForBootstrap: %v", err)
	}
	if string(body) != "hello" {
		t.Fatalf("body = %q", body)
	}
}

func TestReadBoundedFileForBootstrap_EmptyReader(t *testing.T) {
	t.Parallel()
	_, err := readBoundedFileForBootstrap("test", "", strings.NewReader(""))
	if err == nil || !strings.Contains(err.Error(), "non-empty") {
		t.Fatalf("expected non-empty error, got: %v", err)
	}
}

func TestReadBoundedFileForBootstrap_FromFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "testfile")
	if err := os.WriteFile(path, []byte("content"), 0o644); err != nil {
		t.Fatal(err)
	}
	body, err := readBoundedFileForBootstrap("test", path, nil)
	if err != nil {
		t.Fatalf("readBoundedFileForBootstrap: %v", err)
	}
	if string(body) != "content" {
		t.Fatalf("body = %q", body)
	}
}

// ---------------------------------------------------------------------------
// confirmationReader
// ---------------------------------------------------------------------------

func TestConfirmationReader_YesAccepted(t *testing.T) {
	t.Parallel()
	reader := newConfirmationReader(strings.NewReader("yes\n"))
	var output bytes.Buffer
	accepted, err := reader.confirm(&output, "Continue? ")
	if err != nil || !accepted {
		t.Fatalf("accepted = %v, err = %v", accepted, err)
	}
}

func TestConfirmationReader_ShortYAcceptedByConfirmShort(t *testing.T) {
	t.Parallel()
	for _, input := range []string{"y\n", "Y\n", " y \n", "yes\n", "YES\n"} {
		reader := newConfirmationReader(strings.NewReader(input))
		var output bytes.Buffer
		accepted, err := reader.confirmShort(&output, "Continue? ")
		if err != nil || !accepted {
			t.Fatalf("input %q: accepted = %v, err = %v", input, accepted, err)
		}
	}
}

// TestConfirmationReader_StrictConfirmRejectsShortY pins the trust-decision
// property: the strict confirm used for SSH host-key acceptance must reject a
// single "y" — a stray keystroke must never pin an unverified host key.
func TestConfirmationReader_StrictConfirmRejectsShortY(t *testing.T) {
	t.Parallel()
	for _, input := range []string{"y\n", "Y\n", " y \n", "ye\n"} {
		reader := newConfirmationReader(strings.NewReader(input))
		var output bytes.Buffer
		accepted, err := reader.confirm(&output, "Trust? ")
		if err != nil || accepted {
			t.Fatalf("input %q: strict confirm must reject: accepted = %v, err = %v", input, accepted, err)
		}
	}
}

func TestConfirmationReader_NoRejected(t *testing.T) {
	t.Parallel()
	reader := newConfirmationReader(strings.NewReader("no\n"))
	var output bytes.Buffer
	accepted, err := reader.confirm(&output, "Continue? ")
	if err != nil || accepted {
		t.Fatalf("accepted = %v, err = %v", accepted, err)
	}
}

func TestConfirmationReader_EOF(t *testing.T) {
	t.Parallel()
	reader := newConfirmationReader(strings.NewReader(""))
	var output bytes.Buffer
	accepted, err := reader.confirm(&output, "Continue? ")
	if err != nil || accepted {
		t.Fatalf("EOF should not accept: accepted = %v, err = %v", accepted, err)
	}
}

// ---------------------------------------------------------------------------
// waitForRegistration
// ---------------------------------------------------------------------------

func TestWaitForRegistration_NoWait(t *testing.T) {
	t.Parallel()
	bootstrap := UpstreamSSHBootstrap{
		Output: &bytes.Buffer{},
		NoWait: true,
		Probe:  func(context.Context, string, []byte, []byte) error { return nil },
	}
	result := bootstrap.waitForRegistration(context.Background(), "ssh://git@forge.example/acme", "forge.example", "22", nil, nil)
	if result {
		t.Fatal("NoWait should return false immediately")
	}
}

func TestWaitForRegistration_ImmediateSuccess(t *testing.T) {
	t.Parallel()
	bootstrap := UpstreamSSHBootstrap{
		Output:        &bytes.Buffer{},
		NoWait:        false,
		ProbeInterval: 10 * time.Millisecond,
		ProbeWait:     100 * time.Millisecond,
		Probe:         func(context.Context, string, []byte, []byte) error { return nil },
	}
	result := bootstrap.waitForRegistration(context.Background(), "ssh://git@forge.example/acme", "forge.example", "22", nil, nil)
	if !result {
		t.Fatal("expected success when probe immediately passes")
	}
}

func TestWaitForRegistration_ContextCancelled(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // Cancel immediately.
	bootstrap := UpstreamSSHBootstrap{
		Output:        &bytes.Buffer{},
		ProbeInterval: 10 * time.Millisecond,
		ProbeWait:     100 * time.Millisecond,
		Probe:         func(context.Context, string, []byte, []byte) error { return errors.New("not ready") },
	}
	result := bootstrap.waitForRegistration(ctx, "ssh://git@forge.example/acme", "forge.example", "22", nil, nil)
	if result {
		t.Fatal("expected false when context is cancelled")
	}
}

func TestWaitForRegistration_Timeout(t *testing.T) {
	t.Parallel()
	bootstrap := UpstreamSSHBootstrap{
		Output:        &bytes.Buffer{},
		ProbeInterval: 5 * time.Millisecond,
		ProbeWait:     20 * time.Millisecond,
		Probe:         func(context.Context, string, []byte, []byte) error { return errors.New("not ready") },
	}
	result := bootstrap.waitForRegistration(context.Background(), "ssh://git@forge.example/acme", "forge.example", "22", nil, nil)
	if result {
		t.Fatal("expected false on timeout")
	}
}

// ---------------------------------------------------------------------------
// Upstream.DiscoverRepository edge case
// ---------------------------------------------------------------------------

func TestDiscoverRepository_NilCatalog(t *testing.T) {
	t.Parallel()
	_, err := (Upstreams{}).DiscoverRepository(context.Background(), "repo")
	if err == nil || !strings.Contains(err.Error(), "upstream catalog is required") {
		t.Fatalf("expected catalog-required error, got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Upstream.Remote edge case
// ---------------------------------------------------------------------------

func TestRemote_NilCatalog(t *testing.T) {
	t.Parallel()
	_, err := (Upstreams{}).Remote("repo")
	if err == nil || !strings.Contains(err.Error(), "upstream catalog is required") {
		t.Fatalf("expected catalog-required error, got: %v", err)
	}
}
