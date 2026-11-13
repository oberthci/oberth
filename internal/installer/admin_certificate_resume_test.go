package installer

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/oberthci/oberth/internal/clientcert"
)

func adminResumeFixture(t *testing.T) (Config, []byte, ed25519.PrivateKey) {
	t.Helper()
	public, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	bootstrapPrivateKey(t, key)
	sshKey, err := ssh.NewPublicKey(public)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{}, key)
	if err != nil {
		t.Fatal(err)
	}
	csr := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: der})
	files, err := clientcert.GenerateAuthority()
	if err != nil {
		t.Fatal(err)
	}
	bootstrapCredentialInventory(t, files["ca.key"])
	ca, err := clientcert.Load(files["ca.crt"], files["ca.key"])
	if err != nil {
		t.Fatal(err)
	}
	cert, _, err := ca.Issue("admin@workstation", csr, 0)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "client.csr")
	pubPath := filepath.Join(dir, "workstation.pub")
	for p, data := range map[string][]byte{path: csr, pubPath: ssh.MarshalAuthorizedKey(sshKey)} {
		if err := os.WriteFile(p, data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	cfg := Config{ForgeURL: "ssh://git@github.com/acme", AdminSSHPublicKeyPath: pubPath, AdminCSRPath: path, UplinkIdentity: "admin@workstation", ClientAccess: "none", Upgrade: true}
	return cfg, cert, key
}

type certificateResumeHost struct {
	deps    InstallDeps
	output  bytes.Buffer
	runner  *fakeBaoRunner
	applied bool
	port    string
}

// The actual host/install pipeline runs; only Kubernetes and child transports
// are simulated. The existing PKI is inspected using the recovery-file token.
func newCertificateResumeHost(t *testing.T, onboard func(context.Context, []byte, string, ...string) ([]byte, bool)) *certificateResumeHost {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	_, port, _ := net.SplitHostPort(listener.Addr().String())
	h := &certificateResumeHost{port: port}
	kube := argoSecurityTestClient(currentDeployment(), runningOpenBaoPod(), runningOpenBaoStatefulSet(), preflightNode(), preflightStorage(), readyOberthPod(), readyArgoControllerPod())
	pki, runner := pkiLifecycleDeps(t, []string{"ci.example.com", "localhost", "node", "node.local"}, []string{"192.0.2.1", "127.0.0.1", "::1"})
	h.runner = runner
	h.deps = InstallDeps{Output: &h.output, LookPath: func(s string) (string, error) { return s, nil }, ProbeNetworkPolicy: enforcedPolicyFixture,
		LoadKubeConfig: func(string) (kubernetes.Interface, *rest.Config, string, error) {
			return kube, &rest.Config{Host: "https://192.0.2.10:6443", TLSClientConfig: rest.TLSClientConfig{CAData: newTestServerCA(t, []string{"api.example.test"})}}, "chosen", nil
		},
		RunCommand: func(ctx context.Context, input []byte, name string, args ...string) ([]byte, error) {
			if onboard != nil {
				if out, handled := onboard(ctx, input, name, args...); handled {
					return out, nil
				}
			}
			joined := strings.Join(args, " ")
			if strings.Contains(joined, "openbao-0") || strings.Contains(joined, "admin read-file") {
				return pki.RunCommand(ctx, input, name, args...)
			}
			if strings.Contains(joined, "upstream list") {
				return []byte("NAME KIND URL KEY FINGERPRINT\nforge ssh ssh://git@forge.example.test/acme SHA256:public\n"), nil
			}
			return preflightStats(ctx, input, name, args...)
		},
		RunHelm: func(_ context.Context, args []string) ([]byte, error) {
			joined := " " + strings.Join(args, " ") + " "
			if strings.Contains(joined, " list ") {
				return []byte(`[{"name":"oberth","status":"deployed","chart":"oberth-0.21.10"}]`), nil
			}
			if strings.Contains(joined, " get values ") {
				return []byte(`{"tls":{"pki":{"enabled":true},"extraDNSNames":["ci.example.com"]},"secretstore":{"address":"https://openbao.openbao.svc:8200"},"networkPolicy":{"enabled":true}}`), nil
			}
			if strings.Contains(joined, " show ") {
				return []byte(fakeChartValuesYAML), nil
			}
			if strings.Contains(joined, "--dry-run=server") {
				return []byte(`{"config":{}}`), nil
			}
			if strings.Contains(joined, " upgrade ") && strings.Contains(joined, " oberth ") {
				h.applied = true
			}
			return nil, nil
		}}
	return h
}

func TestIssuedAdminCertificateSurvivesRegistrationFailureAndResume(t *testing.T) {
	t.Setenv(baoTokenEnvVar, "")
	t.Setenv(vaultTokenEnvVar, "")
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	cfg, cert, _ := adminResumeFixture(t)
	pod := &fakeOberthHost{t: t, uplinkOutput: string(cert), addResults: []fakeAddResult{{out: "forge unavailable", err: errors.New("registration refused")}}}
	host := newCertificateResumeHost(t, func(ctx context.Context, input []byte, name string, args ...string) ([]byte, bool) {
		joined := strings.Join(args, " ")
		if strings.Contains(joined, "uplink add") || strings.Contains(joined, "uplink cert") || strings.Contains(joined, "enroll") {
			t.Fatal("resume reissued a certificate or minted another uplink")
		}
		if strings.Contains(joined, "admin read-file /run/oberth-identities/upstream/") {
			return cfg.adminSSHPublicKey, true
		}
		if strings.Contains(joined, "upstream list") || strings.Contains(joined, "upstream add") {
			out, err := pod.run(ctx, input, name, args...)
			if err != nil {
				t.Fatal(err)
			}
			return out, true
		}
		return nil, false
	})
	cfg.ContextName = "chosen"
	cfg.ChartPath = testLocalChart
	cfg.ImageRef = fakeChartImageRef
	cfg.ClientHost = "127.0.0.1"
	cfg.ClientHTTPSPort = host.port
	cfg.Timeout = time.Second
	if err := prepareInstallInputs(t.Context(), &cfg, InstallDeps{}); err != nil {
		t.Fatal(err)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	deps := onboardingDeps(t, pod, &output, strings.NewReader(""), false)
	deps.ContextName = cfg.ContextName
	if err := finishInstallTest(t.Context(), cfg, deps, &output); err == nil || pod.addCalls == 0 {
		t.Fatal("registration did not fail after enrollment", err)
	}
	before, err := os.Stat(cfg.AdminCSRPath + ".crt")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(mustRead(t, cfg.AdminCSRPath+".crt"), cert) || !strings.Contains(strings.Join(pod.uplinkArgv, " "), "--csr -") || !strings.Contains(strings.Join(pod.uplinkArgv, " "), "--admin") || strings.Contains(strings.Join(pod.uplinkArgv, " "), "--bearer") {
		t.Fatal("first attempt did not issue the admin certificate")
	}
	firstAdds := pod.addCalls
	pod.addResults = []fakeAddResult{{registers: true}}
	const token = "s.certificate-resume-fixture"
	bootstrapCredentialInventory(t, []byte(token))
	cfg.ResumeSecretsFile = filepath.Join(t.TempDir(), "saved")
	cfg.SecretsFile = filepath.Join(t.TempDir(), "new")
	recovery := []byte("OPENBAO_ROOT_TOKEN=" + token + "\n")
	if err := os.WriteFile(cfg.ResumeSecretsFile, recovery, 0600); err != nil {
		t.Fatal(err)
	}
	// The production-store recipe must accept the same retained admin flags
	// before it contacts Kubernetes, too; it must not demand another CSR.
	production := cfg
	production.InstallSecretStore = true
	if err := Execute(t.Context(), production, noClusterInstallDeps(t, false)); err == nil || !strings.Contains(err.Error(), "stop: cluster reached") {
		t.Fatal("production resume refused its retained certificate", err)
	}
	if err := Execute(t.Context(), cfg, host.deps); err != nil {
		t.Fatal("resume failed", err, host.output.String())
	}
	after, err := os.Stat(cfg.AdminCSRPath + ".crt")
	if err != nil {
		t.Fatal(err)
	}
	if pod.addCalls != firstAdds+1 || !host.applied || !os.SameFile(before, after) || !before.ModTime().Equal(after.ModTime()) || !bytes.Equal(mustRead(t, cfg.AdminCSRPath+".crt"), cert) {
		t.Fatal("resume did not finish registration while preserving the certificate")
	}
	if !bytes.Equal(mustRead(t, cfg.ResumeSecretsFile), recovery) || strings.Contains(host.output.String(), token) {
		t.Fatal("resume changed or disclosed recovery input")
	}
}

func TestAdminCertificateReuseRejectsUnrelatedOrInvalidOutput(t *testing.T) {
	for _, test := range []string{"different_csr", "different_identity", "expired", "server_usage", "malformed", "symlink"} {
		t.Run(test, func(t *testing.T) {
			cfg, cert, key := adminResumeFixture(t)
			if test == "different_csr" {
				_, other, err := ed25519.GenerateKey(rand.Reader)
				if err != nil {
					t.Fatal(err)
				}
				bootstrapPrivateKey(t, other)
				der, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{}, other)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(cfg.AdminCSRPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: der}), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if test == "different_identity" {
				cfg.UplinkIdentity = "other@workstation"
			}
			if test == "expired" || test == "server_usage" {
				block, _ := pem.Decode(cert)
				leaf, err := x509.ParseCertificate(block.Bytes)
				if err != nil {
					t.Fatal(err)
				}
				if test == "expired" {
					leaf.NotBefore = time.Now().Add(-2 * time.Hour)
					leaf.NotAfter = time.Now().Add(-time.Hour)
				} else {
					leaf.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}
				}
				der, err := x509.CreateCertificate(rand.Reader, leaf, leaf, key.Public(), key)
				if err != nil {
					t.Fatal(err)
				}
				cert = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
			}
			if test == "malformed" {
				cert = []byte("not a certificate")
			}
			path := cfg.AdminCSRPath + ".crt"
			if test == "symlink" {
				if err := os.WriteFile(path+".target", cert, 0600); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(path+".target", path); err != nil {
					t.Fatal(err)
				}
			} else if err := os.WriteFile(path, cert, 0600); err != nil {
				t.Fatal(err)
			}
			err := prepareInstallInputs(t.Context(), &cfg, InstallDeps{})
			var diagnostic *installStepError
			if !errors.As(err, &diagnostic) || diagnostic.check.Code != "admin_certificate" {
				t.Fatal("unsafe existing certificate was reused or lacked a public refusal reason", err)
			}
			if !bytes.Equal(mustRead(t, path), cert) {
				t.Fatal("refusal changed the existing output")
			}
		})
	}
}

func TestAdminCertificateResumeRecoversItsOriginalIdentity(t *testing.T) {
	cfg, cert, _ := adminResumeFixture(t)
	cfg.UplinkIdentity = ""
	if err := os.WriteFile(cfg.AdminCSRPath+".crt", cert, 0600); err != nil {
		t.Fatal(err)
	}
	if err := prepareInstallInputs(t.Context(), &cfg, InstallDeps{}); err != nil {
		t.Fatal(err)
	}
	if !cfg.uplinkOnboarded || cfg.UplinkIdentity != "admin@workstation" || !bytes.Equal(mustRead(t, cfg.AdminCSRPath+".crt"), cert) {
		t.Fatal("resume replaced the original enrollment identity or output")
	}
}
