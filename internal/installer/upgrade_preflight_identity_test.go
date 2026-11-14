package installer

import (
	"crypto/tls"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/oberthci/oberth/internal/clientcert"
)

func TestUpgradePreflightRefusalNamesLocalIdentity(t *testing.T) {
	for _, kind := range []string{"bearer token", "certificate files", "cached installer certificate"} {
		t.Run(kind, func(t *testing.T) { testUpgradePreflightLocalIdentity(t, kind) })
	}
}

func testUpgradePreflightLocalIdentity(t *testing.T, kind string) {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	for _, name := range []string{"OBERTH_MCP_URL", "OBERTH_TOKEN", "OBERTH_TOKEN_COMMAND", "OBERTH_CLIENT_CERT", "OBERTH_CLIENT_KEY", "OBERTH_CA_CERT"} {
		t.Setenv(name, "")
	}
	var requests atomic.Int32
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.URL.Path != "/api/admin/version" || r.Method != http.MethodPost {
			t.Error("unexpected preflight call", r.Method, r.URL.Path)
		}
		if kind == "bearer token" {
			if r.Header.Get("Authorization") != "Bearer fixture-secret" {
				t.Error("missing configured bearer token")
			}
		} else if len(r.TLS.PeerCertificates) != 1 || r.TLS.PeerCertificates[0].Subject.CommonName != "scoped-admin@host" {
			t.Error("missing configured client certificate")
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":"requires an unscoped admin identity"}`))
	}))
	server.TLS = &tls.Config{ClientAuth: tls.RequestClientCert}
	server.StartTLS()
	defer server.Close()
	root := t.TempDir()
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	serverCA := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})
	files := map[string][]byte{"server-ca.crt": serverCA}
	f := newUpgradeFixture(t, "0.22.7", "0.22.8", "99")
	f.deps.Admin = nil
	f.deps.RestConfig = nil
	f.deps.ClientConfigRoot = func() (string, error) { return root, nil }
	want := "bearer token"
	t.Setenv("OBERTH_BASE_URL", server.URL)
	t.Setenv("OBERTH_CA_CERT", filepath.Join(root, "server-ca.crt"))
	if kind == "bearer token" {
		t.Setenv("OBERTH_TOKEN", "fixture-secret")
	} else {
		enrollment, err := newInstallerEnrollment()
		if err != nil {
			t.Fatal(err)
		}
		defer clear(enrollment.key)
		authorityFiles, err := clientcert.GenerateAuthority()
		if err != nil {
			t.Fatal(err)
		}
		defer clear(authorityFiles["ca.key"])
		authority, err := clientcert.Load(authorityFiles["ca.crt"], authorityFiles["ca.key"])
		if err != nil {
			t.Fatal(err)
		}
		certificate, _, err := authority.Issue("scoped-admin@host", enrollment.csr, 0)
		if err != nil {
			t.Fatal(err)
		}
		want = "CN=scoped-admin@host"
		if kind == "certificate files" {
			files["client.crt"], files["client.key"] = certificate, enrollment.key
			t.Setenv("OBERTH_CLIENT_CERT", filepath.Join(root, "client.crt"))
			t.Setenv("OBERTH_CLIENT_KEY", filepath.Join(root, "client.key"))
		} else {
			files["installer-client.crt"], files["installer-client.key"], files["installer-ca.crt"] = certificate, enrollment.key, serverCA
			files["installer-target.json"], err = json.Marshal(credentialTarget(Config{Namespace: DefaultNamespace}, f.deps))
			if err != nil {
				t.Fatal(err)
			}
		}
	}
	for name, data := range files {
		if err := os.WriteFile(filepath.Join(root, name), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	// Cached credentials use the internal server DNS name. Validate their
	// locally resolved subject without contacting that address.
	if kind == "cached installer certificate" {
		configured, err := checkUpgradeClientConfiguration(t.Context(), UpgradeConfig{Namespace: DefaultNamespace}, f.deps)
		if err != nil || upgradeAdminClientIdentity(configured) != want {
			t.Fatal("cached certificate identity missing", err, upgradeAdminClientIdentity(configured))
		}
		return
	}
	if _, err := f.deps.KubeClient.CoreV1().Pods(DefaultNamespace).Create(t.Context(), upgradePreflightPod("server", corev1.PodRunning), metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	writes, _ := stopUpgradeAtHelmWrite(&f.deps)
	_, err := RunUpgrade(t.Context(), UpgradeConfig{BinaryVersion: "0.22.8", Namespace: DefaultNamespace, NoBackup: true, RecordDir: t.TempDir()}, f.deps)
	if err == nil || *writes != 0 || requests.Load() != 1 || !strings.Contains(err.Error(), "locally configured identity: "+want) || !strings.Contains(err.Error(), "requires an unscoped admin identity") {
		t.Fatal("403 refusal lost local identity or server detail", *writes, requests.Load(), err)
	}
	if strings.Contains(err.Error()+f.output.String(), "fixture-secret") {
		t.Fatal("refusal exposed the bearer token")
	}
}
