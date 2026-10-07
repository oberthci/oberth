package installer

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

// testCertAndKey generates a self-signed ECDSA P-256 certificate and private
// key with the given DNS names. Returns PEM-encoded cert and key.
func testCertAndKey(t *testing.T, dnsNames []string) (certPEM, keyPEM []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	serial, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "oberth"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(10 * 365 * 24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		BasicConstraintsValid: true,
		DNSNames:              dnsNames,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
	return certPEM, keyPEM
}

// legacyDeployment returns a Deployment with the five Secret volumes that a
// pre-v0.17.10 Oberth installation has.
func legacyDeployment() *appsv1.Deployment {
	d := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "oberth", Namespace: "oberth"}}
	d.Spec.Template.Spec.Volumes = []corev1.Volume{
		{Name: "tls", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: "oberth-tls"}}},
		{Name: "host-key", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: "oberth-ssh-host-key"}}},
		{Name: "upstream-key", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: "oberth-upstream-key"}}},
		{Name: "known-hosts", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: "oberth-known-hosts"}}},
		{Name: "goproxy-tls", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: "oberth-goproxy-tls"}}},
	}
	return d
}

// currentDeployment returns a Deployment without Secret volumes (post-migration).
func currentDeployment() *appsv1.Deployment {
	d := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "oberth", Namespace: "oberth"}}
	d.Spec.Template.Spec.Volumes = []corev1.Volume{
		{Name: "identities", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{Medium: corev1.StorageMediumMemory}}},
	}
	return d
}

// baoIdentityJSON builds a bao read -format=json response for an identity bundle.
func baoIdentityJSON(version int, data map[string]string) string {
	envelope := map[string]any{
		"data": map[string]any{
			"data":     data,
			"metadata": map[string]any{"version": float64(version)},
		},
	}
	b, _ := json.Marshal(envelope)
	return string(b)
}

// preflightRunner scripts both bao commands (via kubectl exec into openbao-0)
// and kubectl exec into the oberth deployment (for pod cert reads).
type preflightRunner struct {
	t           *testing.T
	baoRunner   *fakeBaoRunner
	podCertPEM  []byte   // returned for kubectl exec into deploy/oberth
	podCertFail bool     // if true, kubectl exec into deploy/oberth fails
	calls       []string // all non-bao kubectl exec calls
}

func (r *preflightRunner) run(ctx context.Context, input []byte, name string, args ...string) ([]byte, error) {
	r.t.Helper()
	if name != "kubectl" {
		r.t.Fatalf("unexpected host command %q", name)
	}
	// Dispatch: bao commands go to baoRunner; pod cert reads are handled here.
	for _, arg := range args {
		if arg == "deploy/oberth" {
			r.calls = append(r.calls, strings.Join(args, " "))
			if r.podCertFail {
				return nil, fmt.Errorf("exec into deploy/oberth failed")
			}
			return r.podCertPEM, nil
		}
	}
	return r.baoRunner.run(ctx, input, name, args...)
}

func TestPreflightLegacyBundleAbsentRefusesBeforeHelm(t *testing.T) {
	t.Setenv(baoTokenEnvVar, "test-root-token")

	runner := &fakeBaoRunner{t: t, responses: map[string]fakeBaoResponse{
		// server bundle is absent (No value found)
		"read -format=json " + defaultKVPrefix + "/data/identities/oberth/server": {
			out: "No value found", err: fmt.Errorf("exit 2"),
		},
		// The other bundles are present — preflight reads ALL before reporting.
		"read -format=json " + defaultKVPrefix + "/data/identities/oberth/oberth-upstream-key": {
			out: baoIdentityJSON(1, map[string]string{"id_ed25519": "priv", "id_ed25519.pub": "pub"}),
		},
		"read -format=json " + defaultKVPrefix + "/data/identities/oberth/oberth-known-hosts": {
			out: baoIdentityJSON(1, map[string]string{"known_hosts": "known"}),
		},
	}}
	pr := &preflightRunner{t: t, baoRunner: runner}

	var buf bytes.Buffer
	deps := Deps{
		Output:      &buf,
		KubeClient:  fake.NewClientset(legacyDeployment(), runningOpenBaoPod()),
		RunCommand:  pr.run,
		ContextName: "test-ctx",
	}

	cfg := Config{InstallSecretStore: true}
	err := preflightServerIdentities(context.Background(), &cfg, deps, false)
	if err == nil || !strings.Contains(err.Error(), "seeded in OpenBao") {
		t.Fatalf("expected seeding error, got: %v", err)
	}
	if !strings.Contains(err.Error(), "server") {
		t.Fatalf("error should name the missing bundle: %v", err)
	}
}

func TestPreflightLegacyAllBundlesPresentCertCoversNames(t *testing.T) {
	t.Setenv(baoTokenEnvVar, "test-root-token")

	certPEM, keyPEM := testCertAndKey(t, []string{
		"oberth", "oberth.oberth", "oberth.oberth.svc", "oberth.oberth.svc.cluster.local",
		"oberth-goproxy", "oberth-goproxy.oberth", "oberth-goproxy.oberth.svc", "oberth-goproxy.oberth.svc.cluster.local",
	})

	runner := &fakeBaoRunner{t: t, responses: map[string]fakeBaoResponse{
		"read -format=json " + defaultKVPrefix + "/data/identities/oberth/server": {
			out: baoIdentityJSON(1, map[string]string{"tls.crt": string(certPEM), "tls.key": string(keyPEM), "ssh_host_key": "fake-ssh-key"}),
		},
		"read -format=json " + defaultKVPrefix + "/data/identities/oberth/oberth-upstream-key": {
			out: baoIdentityJSON(1, map[string]string{"id_ed25519": "privkey", "id_ed25519.pub": "pubkey"}),
		},
		"read -format=json " + defaultKVPrefix + "/data/identities/oberth/oberth-known-hosts": {
			out: baoIdentityJSON(1, map[string]string{"known_hosts": "github.com ssh-ed25519 AAAA...\n"}),
		},
	}}
	pr := &preflightRunner{t: t, baoRunner: runner, podCertPEM: certPEM}

	helmCalls := 0
	var buf bytes.Buffer
	deps := Deps{
		Output:     &buf,
		KubeClient: fake.NewClientset(legacyDeployment(), runningOpenBaoPod()),
		RunCommand: pr.run,
		RunHelm: func(_ context.Context, args []string) ([]byte, error) {
			helmCalls++
			return nil, nil
		},
		ContextName: "test-ctx",
	}

	cfg := Config{InstallSecretStore: true}
	err := preflightServerIdentities(context.Background(), &cfg, deps, false)
	if err != nil {
		t.Fatal(err)
	}
	// No Helm calls should have been made by the preflight itself.
	// The helm call from readLiveHelmValuesSubset is expected (returns nil → goProxy disabled).
	// But this test scenario has no goproxy SANs missing, so no write.
}

func TestPreflightCertLacksGoProxySANsRotates(t *testing.T) {
	t.Setenv(baoTokenEnvVar, "test-root-token")

	// Cert with only server names, no goproxy names.
	certPEM, keyPEM := testCertAndKey(t, []string{
		"oberth", "oberth.oberth", "oberth.oberth.svc", "oberth.oberth.svc.cluster.local",
	})

	liveValues := liveHelmValuesSubset{}
	liveValues.Argo.GoProxy.Enabled = true
	liveValuesJSON, _ := json.Marshal(liveValues)

	serverV1 := baoIdentityJSON(1, map[string]string{
		"tls.crt": string(certPEM), "tls.key": string(keyPEM), "ssh_host_key": "fake-ssh-key",
	})

	// Track server reads to return v2 on the readback after the kv patch.
	serverReadCount := 0
	var patchSeen bool
	var patchAuthenticated bool

	runner := func(ctx context.Context, input []byte, name string, args ...string) ([]byte, error) {
		if name != "kubectl" {
			t.Fatalf("unexpected command %q", name)
		}
		// Pod cert reads (deploy/oberth)
		for _, arg := range args {
			if arg == "deploy/oberth" {
				return certPEM, nil
			}
		}
		// Extract the bao-level command
		command, authenticated, err := stripKubectlBaoPlumbing(args)
		if err != nil {
			t.Fatalf("%v", err)
		}
		switch {
		case command == "read -format=json "+defaultKVPrefix+"/data/identities/oberth/server":
			serverReadCount++
			if patchSeen {
				// Readback after rotation: return version 2 with the same key/ssh,
				// but the cert will be the rotated one. Use the original cert as a
				// stand-in (the test is primarily asserting the write was issued and
				// version incremented; the actual rotation is tested in
				// TestRotateCertificatePreservesKeyAndAddsNames).
				return []byte(baoIdentityJSON(2, map[string]string{
					"tls.crt": string(certPEM), "tls.key": string(keyPEM), "ssh_host_key": "fake-ssh-key",
				})), nil
			}
			return []byte(serverV1), nil
		case command == "read -format=json "+defaultKVPrefix+"/data/identities/oberth/oberth-upstream-key":
			return []byte(baoIdentityJSON(1, map[string]string{"id_ed25519": "priv", "id_ed25519.pub": "pub"})), nil
		case command == "read -format=json "+defaultKVPrefix+"/data/identities/oberth/oberth-known-hosts":
			return []byte(baoIdentityJSON(1, map[string]string{"known_hosts": "known\n"})), nil
		case strings.HasPrefix(command, "kv patch"):
			patchSeen = true
			patchAuthenticated = authenticated
			return []byte(`{"data":{"version":2}}`), nil
		default:
			t.Fatalf("unscripted command %q (auth=%v)", command, authenticated)
			return nil, nil
		}
	}

	var buf bytes.Buffer
	deps := Deps{
		Output:     &buf,
		KubeClient: fake.NewClientset(legacyDeployment(), runningOpenBaoPod()),
		RunCommand: runner,
		RunHelm: func(_ context.Context, args []string) ([]byte, error) {
			if len(args) > 0 && args[0] == "get" {
				return liveValuesJSON, nil
			}
			return nil, nil
		},
		ContextName: "test-ctx",
	}

	cfg := Config{InstallSecretStore: true}
	err := preflightServerIdentities(context.Background(), &cfg, deps, false)
	if err != nil {
		t.Fatalf("preflight failed: %v", err)
	}
	if !patchSeen {
		t.Fatal("expected a kv patch call for certificate rotation")
	}
	if !patchAuthenticated {
		t.Fatal("kv patch was not authenticated")
	}
}

func TestPreflightDeploymentWithoutSecretVolumesSkipsBundleCheck(t *testing.T) {
	t.Setenv(baoTokenEnvVar, "")

	var buf bytes.Buffer
	deps := Deps{
		Output:     &buf,
		KubeClient: fake.NewClientset(currentDeployment()),
		RunHelm: func(_ context.Context, _ []string) ([]byte, error) {
			return nil, nil
		},
	}

	cfg := Config{}
	err := preflightServerIdentities(context.Background(), &cfg, deps, false)
	if err != nil {
		t.Fatal(err)
	}
}

func TestPreflightNoDeploymentSkips(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	deps := Deps{
		Output:     &buf,
		KubeClient: fake.NewClientset(),
	}

	cfg := Config{}
	err := preflightServerIdentities(context.Background(), &cfg, deps, false)
	if err != nil {
		t.Fatal(err)
	}
}

func TestPreflightOpenBaoCertMismatchRefuses(t *testing.T) {
	t.Setenv(baoTokenEnvVar, "test-root-token")

	baoCertPEM, _ := testCertAndKey(t, []string{"oberth"})
	podCertPEM, _ := testCertAndKey(t, []string{"oberth"}) // different key

	runner := &fakeBaoRunner{t: t, responses: map[string]fakeBaoResponse{
		"read -format=json " + defaultKVPrefix + "/data/identities/oberth/server": {
			out: baoIdentityJSON(1, map[string]string{"tls.crt": string(baoCertPEM), "tls.key": "key", "ssh_host_key": "ssh"}),
		},
		"read -format=json " + defaultKVPrefix + "/data/identities/oberth/oberth-upstream-key": {
			out: baoIdentityJSON(1, map[string]string{"id_ed25519": "privkey", "id_ed25519.pub": "pubkey"}),
		},
		"read -format=json " + defaultKVPrefix + "/data/identities/oberth/oberth-known-hosts": {
			out: baoIdentityJSON(1, map[string]string{"known_hosts": "known"}),
		},
	}}
	pr := &preflightRunner{t: t, baoRunner: runner, podCertPEM: podCertPEM}

	var buf bytes.Buffer
	deps := Deps{
		Output:      &buf,
		KubeClient:  fake.NewClientset(legacyDeployment(), runningOpenBaoPod()),
		RunCommand:  pr.run,
		ContextName: "test-ctx",
	}

	cfg := Config{InstallSecretStore: true}
	err := preflightServerIdentities(context.Background(), &cfg, deps, false)
	if err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("expected cert mismatch error, got: %v", err)
	}
	// Fix 1: the error must contain both leaf SHA-256 fingerprints.
	baoBlock, _ := pem.Decode(baoCertPEM)
	wantBaoFP := certSHA256Fingerprint(baoBlock.Bytes)
	podBlock, _ := pem.Decode(podCertPEM)
	wantPodFP := certSHA256Fingerprint(podBlock.Bytes)
	if !strings.Contains(err.Error(), wantBaoFP) {
		t.Fatalf("error should contain the OpenBao cert fingerprint %s: %v", wantBaoFP, err)
	}
	if !strings.Contains(err.Error(), wantPodFP) {
		t.Fatalf("error should contain the pod cert fingerprint %s: %v", wantPodFP, err)
	}
}

func TestPreflightLegacyShapeNoTokenRefuses(t *testing.T) {
	t.Setenv(baoTokenEnvVar, "")

	var buf bytes.Buffer
	deps := Deps{
		Output:     &buf,
		KubeClient: fake.NewClientset(legacyDeployment()),
	}

	cfg := Config{}
	err := preflightServerIdentities(context.Background(), &cfg, deps, false)
	if err == nil {
		t.Fatal("expected error for legacy shape without token")
	}
	if !strings.Contains(err.Error(), baoTokenEnvVar) {
		t.Fatalf("error should mention %s: %v", baoTokenEnvVar, err)
	}
	if !strings.Contains(err.Error(), "--install-secretstore") {
		t.Fatalf("error should mention --install-secretstore: %v", err)
	}
}

func TestPreflightNeverIssuesNonGetKubeAction(t *testing.T) {
	t.Setenv(baoTokenEnvVar, "test-root-token")

	certPEM, keyPEM := testCertAndKey(t, []string{
		"oberth", "oberth.oberth", "oberth.oberth.svc", "oberth.oberth.svc.cluster.local",
	})

	runner := &fakeBaoRunner{t: t, responses: map[string]fakeBaoResponse{
		"read -format=json " + defaultKVPrefix + "/data/identities/oberth/server": {
			out: baoIdentityJSON(1, map[string]string{"tls.crt": string(certPEM), "tls.key": string(keyPEM), "ssh_host_key": "ssh"}),
		},
		"read -format=json " + defaultKVPrefix + "/data/identities/oberth/oberth-upstream-key": {
			out: baoIdentityJSON(1, map[string]string{"id_ed25519": "priv", "id_ed25519.pub": "pub"}),
		},
		"read -format=json " + defaultKVPrefix + "/data/identities/oberth/oberth-known-hosts": {
			out: baoIdentityJSON(1, map[string]string{"known_hosts": "known"}),
		},
	}}
	pr := &preflightRunner{t: t, baoRunner: runner, podCertPEM: certPEM}

	kube := fake.NewClientset(legacyDeployment(), runningOpenBaoPod())
	var buf bytes.Buffer
	deps := Deps{
		Output:     &buf,
		KubeClient: kube,
		RunCommand: pr.run,
		RunHelm: func(_ context.Context, _ []string) ([]byte, error) {
			return nil, nil
		},
		ContextName: "test-ctx",
	}

	cfg := Config{InstallSecretStore: true}
	_ = preflightServerIdentities(context.Background(), &cfg, deps, false)

	for _, action := range kube.Actions() {
		if action.GetVerb() != "get" {
			t.Fatalf("preflight issued non-get kube action: %s %s", action.GetVerb(), action.GetResource().Resource)
		}
	}
}

func TestPreflightDryRunLegacyAbsentBundleRefuses(t *testing.T) {
	t.Setenv(baoTokenEnvVar, "test-root-token")

	runner := &fakeBaoRunner{t: t, responses: map[string]fakeBaoResponse{
		"read -format=json " + defaultKVPrefix + "/data/identities/oberth/server": {
			out: "No value found", err: fmt.Errorf("exit 2"),
		},
		"read -format=json " + defaultKVPrefix + "/data/identities/oberth/oberth-upstream-key": {
			out: baoIdentityJSON(1, map[string]string{"id_ed25519": "priv", "id_ed25519.pub": "pub"}),
		},
		"read -format=json " + defaultKVPrefix + "/data/identities/oberth/oberth-known-hosts": {
			out: baoIdentityJSON(1, map[string]string{"known_hosts": "known"}),
		},
	}}
	pr := &preflightRunner{t: t, baoRunner: runner}

	var buf bytes.Buffer
	deps := Deps{
		Output:      &buf,
		KubeClient:  fake.NewClientset(legacyDeployment(), runningOpenBaoPod()),
		RunCommand:  pr.run,
		ContextName: "test-ctx",
	}

	cfg := Config{InstallSecretStore: true}
	err := preflightServerIdentities(context.Background(), &cfg, deps, true)
	if err == nil || !strings.Contains(err.Error(), "seeded in OpenBao") {
		t.Fatalf("dry-run should refuse when bundles are absent: %v", err)
	}
}

func TestPreflightDryRunMissingSANsPrintsPlanNoWrite(t *testing.T) {
	t.Setenv(baoTokenEnvVar, "test-root-token")

	// Cert without goproxy names.
	certPEM, keyPEM := testCertAndKey(t, []string{
		"oberth", "oberth.oberth", "oberth.oberth.svc", "oberth.oberth.svc.cluster.local",
	})

	runner := &fakeBaoRunner{t: t, responses: map[string]fakeBaoResponse{
		"read -format=json " + defaultKVPrefix + "/data/identities/oberth/server": {
			out: baoIdentityJSON(1, map[string]string{"tls.crt": string(certPEM), "tls.key": string(keyPEM), "ssh_host_key": "ssh"}),
		},
		"read -format=json " + defaultKVPrefix + "/data/identities/oberth/oberth-upstream-key": {
			out: baoIdentityJSON(1, map[string]string{"id_ed25519": "priv", "id_ed25519.pub": "pub"}),
		},
		"read -format=json " + defaultKVPrefix + "/data/identities/oberth/oberth-known-hosts": {
			out: baoIdentityJSON(1, map[string]string{"known_hosts": "known"}),
		},
	}}
	pr := &preflightRunner{t: t, baoRunner: runner, podCertPEM: certPEM}

	liveValues := liveHelmValuesSubset{}
	liveValues.Argo.GoProxy.Enabled = true
	liveValues.WatchTunnel.Enabled = true
	liveValues.WatchTunnel.OriginCACert = string(certPEM)
	liveValuesJSON, _ := json.Marshal(liveValues)

	var buf bytes.Buffer
	deps := Deps{
		Output:     &buf,
		KubeClient: fake.NewClientset(legacyDeployment(), runningOpenBaoPod()),
		RunCommand: pr.run,
		RunHelm: func(_ context.Context, args []string) ([]byte, error) {
			if len(args) > 0 && args[0] == "get" {
				return liveValuesJSON, nil
			}
			return nil, nil
		},
		ContextName: "test-ctx",
	}

	cfg := Config{InstallSecretStore: true}
	err := preflightServerIdentities(context.Background(), &cfg, deps, true)
	if err != nil {
		t.Fatalf("dry-run should succeed: %v", err)
	}

	output := buf.String()
	if !strings.Contains(output, "would rotate") {
		t.Fatalf("dry-run should print rotation plan: %s", output)
	}
	if !strings.Contains(output, "watchTunnel.originCACert") {
		t.Fatalf("dry-run should mention watchTunnel.originCACert: %s", output)
	}

	// Verify no kv patch was issued (dry-run = no writes).
	for _, call := range runner.calls {
		if strings.Contains(call.command, "kv patch") {
			t.Fatal("dry-run must not write to OpenBao")
		}
	}
}

func TestOberthHelmArgsWatchTunnelOriginCACert(t *testing.T) {
	t.Parallel()

	t.Run("present when set", func(t *testing.T) {
		t.Parallel()
		cfg := Config{watchTunnelOriginCACert: "-----BEGIN CERTIFICATE-----\nrotated\n-----END CERTIFICATE-----\n"}
		args := strings.Join(OberthHelmArgs(cfg, OpenBaoResult{}, RekorResult{}), " ")
		if !strings.Contains(args, "watchTunnel.originCACert=") {
			t.Fatalf("expected watchTunnel.originCACert in args: %s", args)
		}
	})
	t.Run("absent when empty", func(t *testing.T) {
		t.Parallel()
		cfg := Config{}
		args := strings.Join(OberthHelmArgs(cfg, OpenBaoResult{}, RekorResult{}), " ")
		if strings.Contains(args, "watchTunnel.originCACert=") {
			t.Fatalf("unexpected watchTunnel.originCACert in args: %s", args)
		}
	})
}

func TestCertSHA256Fingerprint(t *testing.T) {
	t.Parallel()
	certPEM, _ := testCertAndKey(t, []string{"oberth"})
	block, _ := pem.Decode(certPEM)
	fp := certSHA256Fingerprint(block.Bytes)
	h := sha256.Sum256(block.Bytes)
	expected := make([]string, len(h))
	for i, b := range h {
		expected[i] = fmt.Sprintf("%02X", b)
	}
	want := strings.Join(expected, ":")
	if fp != want {
		t.Fatalf("fingerprint = %s, want %s", fp, want)
	}
}

func TestGoProxyDNSNames(t *testing.T) {
	t.Parallel()
	names := goProxyDNSNames("oberth", "oberth")
	want := []string{
		"oberth-goproxy",
		"oberth-goproxy.oberth",
		"oberth-goproxy.oberth.svc",
		"oberth-goproxy.oberth.svc.cluster.local",
	}
	if len(names) != len(want) {
		t.Fatalf("got %v, want %v", names, want)
	}
	for i := range want {
		if names[i] != want[i] {
			t.Errorf("names[%d] = %q, want %q", i, names[i], want[i])
		}
	}
}

func TestRotateCertificatePreservesKeyAndAddsNames(t *testing.T) {
	t.Parallel()
	certPEM, keyPEM := testCertAndKey(t, []string{"oberth", "oberth.oberth.svc"})

	block, _ := pem.Decode(certPEM)
	cert, _ := x509.ParseCertificate(block.Bytes)

	newCertPEM, err := rotateCertificate(cert, keyPEM, []string{"oberth-goproxy.oberth.svc"})
	if err != nil {
		t.Fatal(err)
	}

	newBlock, _ := pem.Decode(newCertPEM)
	if newBlock == nil {
		t.Fatal("rotated cert is not valid PEM")
	}
	newCert, err := x509.ParseCertificate(newBlock.Bytes)
	if err != nil {
		t.Fatal(err)
	}

	// Verify the new cert has the original and new names.
	for _, want := range []string{"oberth", "oberth.oberth.svc", "oberth-goproxy.oberth.svc"} {
		if !stringSliceContains(newCert.DNSNames, want) {
			t.Errorf("rotated cert missing DNS name %q", want)
		}
	}

	// Verify public key is preserved.
	oldPub, _ := x509.MarshalPKIXPublicKey(cert.PublicKey)
	newPub, _ := x509.MarshalPKIXPublicKey(newCert.PublicKey)
	if hex.EncodeToString(oldPub) != hex.EncodeToString(newPub) {
		t.Error("rotated cert has a different public key")
	}

	// Verify NotAfter is preserved.
	if !newCert.NotAfter.Equal(cert.NotAfter) {
		t.Errorf("NotAfter changed: old=%v new=%v", cert.NotAfter, newCert.NotAfter)
	}

	// Verify BasicConstraints.
	if newCert.IsCA {
		t.Error("rotated cert should not be a CA")
	}
}

func TestReadyCloneInstructionsIncludeQualifiedPath(t *testing.T) {
	for _, alias := range []bool{false, true} {
		var out strings.Builder
		printReadyWithNextSteps(&out, "github", "ssh://git@github.com/oberthci", alias, false)
		if !strings.Contains(out.String(), "github/oberthci/<repo>.git") {
			t.Fatal(out.String())
		}
	}
}

// --- Fix 1 additional tests: cross-check compares public key, not PEM bytes ---

func TestPreflightSameKeyDifferentCertPassesCrossCheck(t *testing.T) {
	// Same key, different cert bytes (e.g. rotated cert with new SANs).
	t.Setenv(baoTokenEnvVar, "test-root-token")

	certPEM, keyPEM := testCertAndKey(t, []string{"oberth"})
	block, _ := pem.Decode(certPEM)
	cert, _ := x509.ParseCertificate(block.Bytes)

	// Rotate the cert (new SANs, same key) to get different PEM bytes.
	rotatedCertPEM, err := rotateCertificate(cert, keyPEM, []string{"oberth.new"})
	if err != nil {
		t.Fatal(err)
	}

	runner := &fakeBaoRunner{t: t, responses: map[string]fakeBaoResponse{
		"read -format=json " + defaultKVPrefix + "/data/identities/oberth/server": {
			out: baoIdentityJSON(1, map[string]string{
				"tls.crt": string(rotatedCertPEM), "tls.key": string(keyPEM), "ssh_host_key": "fake-ssh-key",
			}),
		},
		"read -format=json " + defaultKVPrefix + "/data/identities/oberth/oberth-upstream-key": {
			out: baoIdentityJSON(1, map[string]string{"id_ed25519": "priv", "id_ed25519.pub": "pub"}),
		},
		"read -format=json " + defaultKVPrefix + "/data/identities/oberth/oberth-known-hosts": {
			out: baoIdentityJSON(1, map[string]string{"known_hosts": "known"}),
		},
	}}
	// Pod still serves the OLD cert (different PEM, same key).
	pr := &preflightRunner{t: t, baoRunner: runner, podCertPEM: certPEM}

	var buf bytes.Buffer
	deps := Deps{
		Output:      &buf,
		KubeClient:  fake.NewClientset(legacyDeployment(), runningOpenBaoPod()),
		RunCommand:  pr.run,
		ContextName: "test-ctx",
	}

	cfg := Config{InstallSecretStore: true}
	if err := preflightServerIdentities(context.Background(), &cfg, deps, false); err != nil {
		t.Fatalf("same key with different cert bytes should pass: %v", err)
	}
}

func TestPreflightUnparseableBaoCertRefuses(t *testing.T) {
	t.Setenv(baoTokenEnvVar, "test-root-token")

	_, keyPEM := testCertAndKey(t, []string{"oberth"})
	podCertPEM, _ := testCertAndKey(t, []string{"oberth"})

	runner := &fakeBaoRunner{t: t, responses: map[string]fakeBaoResponse{
		"read -format=json " + defaultKVPrefix + "/data/identities/oberth/server": {
			out: baoIdentityJSON(1, map[string]string{
				"tls.crt": "not-valid-pem", "tls.key": string(keyPEM), "ssh_host_key": "ssh",
			}),
		},
		"read -format=json " + defaultKVPrefix + "/data/identities/oberth/oberth-upstream-key": {
			out: baoIdentityJSON(1, map[string]string{"id_ed25519": "priv", "id_ed25519.pub": "pub"}),
		},
		"read -format=json " + defaultKVPrefix + "/data/identities/oberth/oberth-known-hosts": {
			out: baoIdentityJSON(1, map[string]string{"known_hosts": "known"}),
		},
	}}
	pr := &preflightRunner{t: t, baoRunner: runner, podCertPEM: podCertPEM}

	var buf bytes.Buffer
	deps := Deps{
		Output:      &buf,
		KubeClient:  fake.NewClientset(legacyDeployment(), runningOpenBaoPod()),
		RunCommand:  pr.run,
		ContextName: "test-ctx",
	}

	cfg := Config{InstallSecretStore: true}
	err := preflightServerIdentities(context.Background(), &cfg, deps, false)
	if err == nil {
		t.Fatal("expected error for unparseable OpenBao cert")
	}
	if !strings.Contains(err.Error(), "OpenBao") || !strings.Contains(err.Error(), "not valid") {
		t.Fatalf("error should name the OpenBao side: %v", err)
	}
}

// --- Fix 2 tests: unknown Secret volumes refuse before OpenBao reads ---

func deploymentWithSecretVolumes(names ...string) *appsv1.Deployment {
	d := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "oberth", Namespace: "oberth"}}
	for _, name := range names {
		d.Spec.Template.Spec.Volumes = append(d.Spec.Template.Spec.Volumes, corev1.Volume{
			Name:         name,
			VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: name}},
		})
	}
	return d
}

func TestPreflightUnknownSecretVolumeRefuses(t *testing.T) {
	// No BAO_TOKEN needed — the refusal happens before any token check.
	t.Setenv(baoTokenEnvVar, "")

	var buf bytes.Buffer
	deps := Deps{
		Output:     &buf,
		KubeClient: fake.NewClientset(deploymentWithSecretVolumes("my-custom-tls")),
	}

	cfg := Config{}
	err := preflightServerIdentities(context.Background(), &cfg, deps, false)
	if err == nil {
		t.Fatal("expected refusal for unknown Secret volume")
	}
	if !strings.Contains(err.Error(), "unsupported deployment shape") {
		t.Fatalf("error should say unsupported deployment shape: %v", err)
	}
	if !strings.Contains(err.Error(), "my-custom-tls") {
		t.Fatalf("error should name the unknown volume: %v", err)
	}
}

func TestPreflightLegacyPlusUnknownVolumeRefuses(t *testing.T) {
	t.Setenv(baoTokenEnvVar, "")

	var buf bytes.Buffer
	deps := Deps{
		Output:     &buf,
		KubeClient: fake.NewClientset(deploymentWithSecretVolumes("oberth-tls", "my-custom-tls")),
	}

	cfg := Config{}
	err := preflightServerIdentities(context.Background(), &cfg, deps, false)
	if err == nil {
		t.Fatal("expected refusal for legacy deployment with unknown volume")
	}
	if !strings.Contains(err.Error(), "unsupported deployment shape") {
		t.Fatalf("error should say unsupported deployment shape: %v", err)
	}
	if !strings.Contains(err.Error(), "my-custom-tls") {
		t.Fatalf("error should name the unknown volume: %v", err)
	}
}

// --- Fix 3 tests: only URL name is hard-required for goProxy SAN check ---

func TestPreflightCertWithOnlySvcNameNoRotation(t *testing.T) {
	// Cert has only oberth-goproxy.oberth.svc (the URL name) — no rotation.
	t.Setenv(baoTokenEnvVar, "test-root-token")

	certPEM, keyPEM := testCertAndKey(t, []string{
		"oberth", "oberth.oberth", "oberth.oberth.svc", "oberth.oberth.svc.cluster.local",
		"oberth-goproxy.oberth.svc", // only the URL name
	})

	liveValues := liveHelmValuesSubset{}
	liveValues.Argo.GoProxy.Enabled = true
	liveValuesJSON, _ := json.Marshal(liveValues)

	// Use a current-shape deployment (no Secret volumes) so no legacy checks.
	var buf bytes.Buffer
	deps := Deps{
		Output:     &buf,
		KubeClient: fake.NewClientset(currentDeployment(), runningOpenBaoPod()),
		RunCommand: func(ctx context.Context, input []byte, name string, args ...string) ([]byte, error) {
			// Pod cert reads.
			return certPEM, nil
		},
		RunHelm: func(_ context.Context, args []string) ([]byte, error) {
			if len(args) > 0 && args[0] == "get" {
				return liveValuesJSON, nil
			}
			return nil, nil
		},
		ContextName: "test-ctx",
	}

	// Wire the bao store to serve the server identity.
	baoRunner := &fakeBaoRunner{t: t, responses: map[string]fakeBaoResponse{
		"read -format=json " + defaultKVPrefix + "/data/identities/oberth/server": {
			out: baoIdentityJSON(1, map[string]string{
				"tls.crt": string(certPEM), "tls.key": string(keyPEM), "ssh_host_key": "ssh",
			}),
		},
	}}

	// Override RunCommand to dispatch between pod cert and bao calls.
	deps.RunCommand = func(ctx context.Context, input []byte, name string, args ...string) ([]byte, error) {
		for _, arg := range args {
			if arg == "deploy/oberth" {
				return certPEM, nil
			}
		}
		return baoRunner.run(ctx, input, name, args...)
	}

	cfg := Config{InstallSecretStore: true}
	err := preflightServerIdentities(context.Background(), &cfg, deps, false)
	if err != nil {
		t.Fatalf("cert with the URL name should not trigger rotation: %v", err)
	}
	// Verify no kv patch was issued.
	for _, call := range baoRunner.calls {
		if strings.Contains(call.command, "kv patch") {
			t.Fatal("should not rotate when the URL name is present")
		}
	}
}

func TestPreflightCertWithoutSvcNameRotatesAllFour(t *testing.T) {
	// Cert without the goproxy URL name → rotation adds all four forms.
	t.Setenv(baoTokenEnvVar, "test-root-token")

	certPEM, keyPEM := testCertAndKey(t, []string{
		"oberth", "oberth.oberth", "oberth.oberth.svc", "oberth.oberth.svc.cluster.local",
	})

	liveValues := liveHelmValuesSubset{}
	liveValues.Argo.GoProxy.Enabled = true
	liveValuesJSON, _ := json.Marshal(liveValues)

	patchSeen := false
	serverReadCount := 0

	runner := func(ctx context.Context, input []byte, name string, args ...string) ([]byte, error) {
		if name != "kubectl" {
			t.Fatalf("unexpected command %q", name)
		}
		for _, arg := range args {
			if arg == "deploy/oberth" {
				return certPEM, nil
			}
		}
		command, _, err := stripKubectlBaoPlumbing(args)
		if err != nil {
			t.Fatalf("%v", err)
		}
		switch {
		case command == "read -format=json "+defaultKVPrefix+"/data/identities/oberth/server":
			serverReadCount++
			if patchSeen {
				return []byte(baoIdentityJSON(2, map[string]string{
					"tls.crt": string(certPEM), "tls.key": string(keyPEM), "ssh_host_key": "ssh",
				})), nil
			}
			return []byte(baoIdentityJSON(1, map[string]string{
				"tls.crt": string(certPEM), "tls.key": string(keyPEM), "ssh_host_key": "ssh",
			})), nil
		case strings.HasPrefix(command, "kv patch"):
			patchSeen = true
			return []byte(`{"data":{"version":2}}`), nil
		default:
			t.Fatalf("unscripted command %q", command)
			return nil, nil
		}
	}

	var buf bytes.Buffer
	deps := Deps{
		Output:     &buf,
		KubeClient: fake.NewClientset(currentDeployment(), runningOpenBaoPod()),
		RunCommand: runner,
		RunHelm: func(_ context.Context, args []string) ([]byte, error) {
			if len(args) > 0 && args[0] == "get" {
				return liveValuesJSON, nil
			}
			return nil, nil
		},
		ContextName: "test-ctx",
	}

	cfg := Config{InstallSecretStore: true}
	err := preflightServerIdentities(context.Background(), &cfg, deps, false)
	if err != nil {
		t.Fatalf("rotation should succeed: %v", err)
	}
	if !patchSeen {
		t.Fatal("expected rotation when the URL name is missing")
	}
}

// --- Fix 4 test: Helm error propagates from the preflight ---

func TestPreflightHelmErrorPropagates(t *testing.T) {
	t.Setenv(baoTokenEnvVar, "test-root-token")

	certPEM, keyPEM := testCertAndKey(t, []string{
		"oberth", "oberth.oberth", "oberth.oberth.svc", "oberth.oberth.svc.cluster.local",
	})

	runner := &fakeBaoRunner{t: t, responses: map[string]fakeBaoResponse{
		"read -format=json " + defaultKVPrefix + "/data/identities/oberth/server": {
			out: baoIdentityJSON(1, map[string]string{
				"tls.crt": string(certPEM), "tls.key": string(keyPEM), "ssh_host_key": "ssh",
			}),
		},
	}}

	deps := Deps{
		Output:     &bytes.Buffer{},
		KubeClient: fake.NewClientset(currentDeployment(), runningOpenBaoPod()),
		RunCommand: func(ctx context.Context, input []byte, name string, args ...string) ([]byte, error) {
			for _, arg := range args {
				if arg == "deploy/oberth" {
					return certPEM, nil
				}
			}
			return runner.run(ctx, input, name, args...)
		},
		RunHelm: func(_ context.Context, args []string) ([]byte, error) {
			return nil, fmt.Errorf("helm: HELM_DRIVER=configmap: release not found")
		},
		ContextName: "test-ctx",
	}

	cfg := Config{InstallSecretStore: true}
	err := preflightServerIdentities(context.Background(), &cfg, deps, false)
	if err == nil {
		t.Fatal("expected error when RunHelm fails")
	}
	if !strings.Contains(err.Error(), "read live Helm values for the identity preflight") {
		t.Fatalf("error should mention the identity preflight context: %v", err)
	}
	if !strings.Contains(err.Error(), "release not found") {
		t.Fatalf("error should include the Helm error: %v", err)
	}
}

// --- Fix 6 test: BAO_TOKEN set but pod lookup fails ---

func TestPreflightLegacyTokenSetPodNotFound(t *testing.T) {
	t.Setenv(baoTokenEnvVar, "test-root-token")

	var buf bytes.Buffer
	// No OpenBao pod in the cluster.
	deps := Deps{
		Output:     &buf,
		KubeClient: fake.NewClientset(legacyDeployment()),
	}

	cfg := Config{InstallSecretStore: true}
	err := preflightServerIdentities(context.Background(), &cfg, deps, false)
	if err == nil {
		t.Fatal("expected error for missing OpenBao pod")
	}
	if !strings.Contains(err.Error(), baoTokenEnvVar) {
		t.Fatalf("error should mention %s: %v", baoTokenEnvVar, err)
	}
	if !strings.Contains(err.Error(), "could not be found") {
		t.Fatalf("error should say the pod could not be found: %v", err)
	}
}

// Suppress unused imports for net (used in testCertAndKey via IPAddresses).
var _ = net.ParseIP
var _ = os.Getenv

// TestPreflightClearsIdentityReadBuffers proves issue #811's contract on the
// rotation path: every exec stdout buffer that carried identity material
// (the server bundle, read before and after rotation, and the sibling
// bundles) and every exec stdin buffer (token line, kv patch body) is
// zero-filled by the time preflightServerIdentities returns.
func TestPreflightClearsIdentityReadBuffers(t *testing.T) {
	t.Setenv(baoTokenEnvVar, "test-root-token")

	certPEM, keyPEM := testCertAndKey(t, []string{
		"oberth", "oberth.oberth", "oberth.oberth.svc", "oberth.oberth.svc.cluster.local",
	})
	liveValues := liveHelmValuesSubset{}
	liveValues.Argo.GoProxy.Enabled = true
	liveValuesJSON, _ := json.Marshal(liveValues)

	var handedOut [][]byte
	var stdins [][]byte
	hand := func(s string) []byte {
		b := []byte(s)
		handedOut = append(handedOut, b)
		return b
	}
	patchSeen := false
	runner := func(_ context.Context, input []byte, name string, args ...string) ([]byte, error) {
		if name != "kubectl" {
			t.Fatalf("unexpected command %q", name)
		}
		for _, arg := range args {
			if arg == "deploy/oberth" {
				return certPEM, nil
			}
		}
		command, _, err := stripKubectlBaoPlumbing(args)
		if err != nil {
			t.Fatal(err)
		}
		stdins = append(stdins, input)
		version := 1
		if patchSeen {
			version = 2
		}
		switch {
		case command == "read -format=json "+defaultKVPrefix+"/data/identities/oberth/server":
			return hand(baoIdentityJSON(version, map[string]string{
				"tls.crt": string(certPEM), "tls.key": string(keyPEM), "ssh_host_key": "fake-ssh-key",
			})), nil
		case command == "read -format=json "+defaultKVPrefix+"/data/identities/oberth/oberth-upstream-key":
			return hand(baoIdentityJSON(1, map[string]string{"id_ed25519": "priv", "id_ed25519.pub": "pub"})), nil
		case command == "read -format=json "+defaultKVPrefix+"/data/identities/oberth/oberth-known-hosts":
			return hand(baoIdentityJSON(1, map[string]string{"known_hosts": "known\n"})), nil
		case strings.HasPrefix(command, "kv patch"):
			patchSeen = true
			return []byte(`{"data":{"version":2}}`), nil
		default:
			t.Fatalf("unscripted command %q", command)
			return nil, nil
		}
	}

	var buf bytes.Buffer
	deps := Deps{
		Output:     &buf,
		KubeClient: fake.NewClientset(legacyDeployment(), runningOpenBaoPod()),
		RunCommand: runner,
		RunHelm: func(_ context.Context, args []string) ([]byte, error) {
			if len(args) > 0 && args[0] == "get" {
				return liveValuesJSON, nil
			}
			return nil, nil
		},
		ContextName: "test-ctx",
	}
	cfg := Config{InstallSecretStore: true}
	if err := preflightServerIdentities(context.Background(), &cfg, deps, false); err != nil {
		t.Fatalf("preflight failed: %v", err)
	}
	if !patchSeen {
		t.Fatal("expected a kv patch call for certificate rotation")
	}
	// server (legacy check) + upstream key + known_hosts + server (readback).
	if len(handedOut) < 4 {
		t.Fatalf("expected at least 4 identity reads, got %d", len(handedOut))
	}
	for i, out := range handedOut {
		if !allZero(out) {
			t.Fatalf("identity read buffer %d still holds material after preflight returned", i)
		}
	}
	for i, in := range stdins {
		if !allZero(in) {
			t.Fatalf("exec stdin buffer %d (token line / patch body) was not cleared", i)
		}
	}
	if strings.Contains(buf.String(), string(keyPEM)) {
		t.Fatal("private key reached the output stream")
	}
}
