package main

import (
	"bytes"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/api/fake"
	"github.com/oberthci/oberth/internal/clientcert"
	"github.com/oberthci/oberth/internal/installer"
)

func TestInstallVerifyPrintsItsVerdict(t *testing.T) {
	a, profile := verificationProfile(t)
	out, diagnostic := &bytes.Buffer{}, &bytes.Buffer{}
	a.out, a.diagnostic = out, diagnostic
	dir := filepath.Dir(profile.CAFile)
	for _, valid := range []bool{true, false} {
		out.Reset()
		diagnostic.Reset()
		if !valid {
			profile.CAPin = "wrong"
		}
		writeVerificationProfile(t, dir, profile)
		code := a.run(t.Context(), []string{"install", "--profile=verified", "--verify"})
		if valid {
			if code != 0 || out.String() != "Verifying profile: verified\nVerified: "+profile.ServerURL+"\n" || diagnostic.Len() != 0 {
				t.Fatal(code, out.String(), diagnostic.String())
			}
		} else if code != 1 || out.String() != "Verifying profile: verified\n" || strings.Count(diagnostic.String(), "\n") != 1 || !strings.Contains(diagnostic.String(), "Check the saved trust") {
			t.Fatal(code, out.String(), diagnostic.String())
		}
	}
}

func verificationProfile(t *testing.T) (application, installer.Profile) {
	t.Helper()
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"ready":true,"version":"test"}`))
	}))
	t.Cleanup(server.Close)
	a, _, _ := fixture(t, fake.Bindings())
	a.base = t.TempDir()
	if err := os.Chmod(a.base, 0700); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(a.base, "verified")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	cert := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})
	key, err := x509.MarshalPKCS8PrivateKey(server.TLS.Certificates[0].PrivateKey)
	if err != nil {
		t.Fatal(err)
	}
	profile := installer.Profile{ServerURL: server.URL, ServerVersion: "test", CAPin: clientcert.Pin(server.Certificate()), CAFile: filepath.Join(dir, "ca.crt"), AdminCert: filepath.Join(dir, "admin.crt"), AdminKey: filepath.Join(dir, "admin.key")}
	for name, data := range map[string][]byte{"ca.crt": cert, "admin.crt": cert, "admin.key": pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: key})} {
		if err := os.WriteFile(filepath.Join(dir, name), data, 0600); err != nil {
			t.Fatal(err)
		}
	}

	verificationNamespace(t, &a, &profile, "baseline")
	return a, profile
}

func writeVerificationProfile(t *testing.T, dir string, profile installer.Profile) {
	t.Helper()
	data, err := json.Marshal(profile)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "installation.json"), data, 0600); err != nil {
		t.Fatal(err)
	}
}

func TestInstallVerifyRefusalKeepsFileContext(t *testing.T) {
	a, profile := verificationProfile(t)
	writeVerificationProfile(t, filepath.Dir(profile.CAFile), profile)
	var out, diagnostic bytes.Buffer
	a.out, a.diagnostic = &out, &diagnostic
	unsafe := filepath.Join(a.base, "verified", "installation.json")
	if err := os.Chmod(unsafe, 0666); err != nil {
		t.Fatal(err)
	}
	code := a.run(t.Context(), []string{"install", "--profile=verified", "--verify"})
	if code != 1 || out.Len() != 0 || strings.Count(diagnostic.String(), "\n") != 1 {
		t.Fatal(code, out.String(), diagnostic.String())
	}
	for _, fact := range []string{"Client file is not safe to use.", "Use an owned regular file; remove group and other write permissions.", "File: " + unsafe} {
		if !strings.Contains(diagnostic.String(), fact) {
			t.Fatal("verification omitted a required fact", fact, diagnostic.String())
		}
	}
}

func verificationNamespace(t *testing.T, a *application, profile *installer.Profile, level string) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" || r.URL.Path != "/api/v1/namespaces/oberth-runs" {
			t.Errorf("unexpected cluster request %s %s", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"apiVersion": "v1", "kind": "Namespace", "metadata": map[string]any{"name": "oberth-runs", "labels": map[string]string{"pod-security.kubernetes.io/enforce": level}}})
	}))
	t.Cleanup(server.Close)
	path := filepath.Join(t.TempDir(), "kubeconfig")
	data := `{"apiVersion":"v1","kind":"Config","clusters":[{"name":"test","cluster":{"server":"` + server.URL + `"}}],"contexts":[{"name":"test","context":{"cluster":"test"}}]}`
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	profile.Context, profile.PipelineNamespace = "test", "oberth-runs"
	a.env = func(name string) string {
		if name == "KUBECONFIG" {
			return path
		}
		return ""
	}
}

func TestInstallVerifyRefusesUnsafePipelineNamespace(t *testing.T) {
	for _, level := range []string{"", "privileged", "restricted"} {
		t.Run(level, func(t *testing.T) {
			a, profile := verificationProfile(t)
			verificationNamespace(t, &a, &profile, level)
			writeVerificationProfile(t, filepath.Dir(profile.CAFile), profile)
			var out, diagnostic bytes.Buffer
			a.out, a.diagnostic = &out, &diagnostic
			code := a.run(t.Context(), []string{"install", "--profile=verified", "--verify"})
			if level == "restricted" {
				if code != 0 {
					t.Fatal(code, diagnostic.String())
				}
			} else if code != 1 || out.String() != "Verifying profile: verified\n" || !strings.Contains(diagnostic.String(), "oberth-runs") || !strings.Contains(diagnostic.String(), "pod-security.kubernetes.io/enforce=baseline") {
				t.Fatal("missing namespace refusal", code, out.String(), diagnostic.String())
			}
		})
	}
}
