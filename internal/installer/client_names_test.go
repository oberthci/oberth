package installer

import (
	"bytes"
	"context"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"net"
	"slices"
	"strings"
	"testing"

	"k8s.io/client-go/kubernetes/fake"
)

func TestCertificateNamesForClientHost(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		host             string
		wantDNS, wantIPs []string
	}{
		{"localhost", []string{"localhost"}, []string{"127.0.0.1"}},
		{"", []string{"localhost"}, []string{"127.0.0.1"}},
		{"10.0.0.5", nil, []string{"10.0.0.5"}},
		{"k8s.example.test", []string{"k8s.example.test"}, nil},
	} {
		dns, ips := certificateNamesForClientHost(tc.host, nil, nil)
		if !slices.Equal(dns, tc.wantDNS) || !slices.Equal(ips, tc.wantIPs) {
			t.Errorf("host %q: got %v %v, want %v %v", tc.host, dns, ips, tc.wantDNS, tc.wantIPs)
		}
	}
	// Operator-supplied names keep their order; the advertised host is appended once.
	dns, ips := certificateNamesForClientHost("localhost", []string{"tuxbox", "localhost"}, []string{"192.168.86.193"})
	if !slices.Equal(dns, []string{"tuxbox", "localhost"}) || !slices.Equal(ips, []string{"192.168.86.193", "127.0.0.1"}) {
		t.Fatalf("got %v %v", dns, ips)
	}
}

func TestRotateCertificateAddsIPAddresses(t *testing.T) {
	t.Parallel()
	certPEM, keyPEM := testCertAndKey(t, []string{"oberth"})
	block, _ := pem.Decode(certPEM)
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	rotated, err := rotateCertificate(cert, keyPEM, []string{"localhost"}, []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("192.168.86.193")})
	if err != nil {
		t.Fatal(err)
	}
	newBlock, _ := pem.Decode(rotated)
	newCert, err := x509.ParseCertificate(newBlock.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	if err := newCert.VerifyHostname("127.0.0.1"); err != nil {
		t.Errorf("rotated cert does not cover 127.0.0.1: %v", err)
	}
	if err := newCert.VerifyHostname("192.168.86.193"); err != nil {
		t.Errorf("rotated cert does not cover the node IP: %v", err)
	}
	if err := newCert.VerifyHostname("localhost"); err != nil {
		t.Errorf("rotated cert does not cover localhost: %v", err)
	}
	oldPub, _ := x509.MarshalPKIXPublicKey(cert.PublicKey)
	newPub, _ := x509.MarshalPKIXPublicKey(newCert.PublicKey)
	if !bytes.Equal(oldPub, newPub) {
		t.Fatal("rotation must keep the server key")
	}
}

// A restored or older identity that lacks the addresses clients use is
// re-issued for them (same key) instead of only warning that it never will
// be (observed on tuxbox: the restored certificate carried in-cluster names only).
func TestPreflightRotatesForClientAddresses(t *testing.T) {
	t.Setenv(baoTokenEnvVar, "test-root-token")
	certPEM, keyPEM := testCertAndKey(t, []string{"oberth", "oberth.oberth.svc"})
	liveValuesJSON, _ := json.Marshal(liveHelmValuesSubset{})

	runner := func(_ context.Context, _ []byte, name string, args ...string) ([]byte, error) {
		if slices.Contains(args, "deploy/oberth") {
			return certPEM, nil
		}
		command, _, err := stripKubectlBaoPlumbing(args)
		if err != nil {
			t.Fatal(err)
		}
		if command == "read -format=json "+defaultKVPrefix+"/data/identities/oberth/server" {
			return []byte(baoIdentityJSON(1, map[string]string{
				"tls.crt": string(certPEM), "tls.key": string(keyPEM), "ssh_host_key": "fake-ssh-key",
			})), nil
		}
		t.Fatalf("unscripted command %q", command)
		return nil, nil
	}
	deps := Deps{
		Output:     &bytes.Buffer{},
		KubeClient: fake.NewClientset(currentDeployment(), runningOpenBaoPod()),
		RunCommand: runner,
		RunHelm: func(context.Context, []string) ([]byte, error) {
			return liveValuesJSON, nil
		},
	}
	cfg := Config{
		InstallSecretStore: true,
		TLSExtraDNSNames:   []string{"tuxbox", "localhost"},
		TLSExtraIPs:        []string{"192.168.86.193", "127.0.0.1"},
	}
	if err := preflightServerIdentities(context.Background(), &cfg, deps, false); err != nil {
		t.Fatalf("preflight: %v", err)
	}
	if cfg.pendingCertRotation == nil {
		t.Fatal("expected a pending rotation for the missing client addresses")
	}
	block, _ := pem.Decode(cfg.pendingCertRotation.newCertPEM)
	rotated, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	for _, host := range []string{"tuxbox", "localhost", "192.168.86.193", "127.0.0.1", "oberth.oberth.svc"} {
		if err := rotated.VerifyHostname(host); err != nil {
			t.Errorf("rotated cert does not cover %s: %v", host, err)
		}
	}
}

// Without an OpenBao admin token only a warning is possible; missing client
// addresses must not fail the install the way missing goproxy names do.
func TestPreflightWithoutStoreLeavesClientAddressesToWarning(t *testing.T) {
	t.Setenv(baoTokenEnvVar, "")
	certPEM, _ := testCertAndKey(t, []string{"oberth"})
	liveValuesJSON, _ := json.Marshal(liveHelmValuesSubset{})
	deps := Deps{
		Output:     &bytes.Buffer{},
		KubeClient: fake.NewClientset(currentDeployment()),
		RunCommand: func(_ context.Context, _ []byte, _ string, args ...string) ([]byte, error) {
			if slices.Contains(args, "deploy/oberth") {
				return certPEM, nil
			}
			t.Fatalf("unexpected command %s", strings.Join(args, " "))
			return nil, nil
		},
		RunHelm: func(context.Context, []string) ([]byte, error) { return liveValuesJSON, nil },
	}
	cfg := Config{TLSExtraIPs: []string{"127.0.0.1"}}
	if err := preflightServerIdentities(context.Background(), &cfg, deps, false); err != nil {
		t.Fatalf("preflight must not fail: %v", err)
	}
	if cfg.pendingCertRotation != nil {
		t.Fatal("no rotation is possible without the store")
	}
}
