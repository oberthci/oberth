//go:build linux

// These tests run the built release tool against a local registry.
package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/releaseverify"
)

func TestPublishProvesChartImagesBeforeWriting(t *testing.T) {
	var status atomic.Int32
	var requests atomic.Int32
	registry := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" || (r.Method != "GET" && r.Method != "HEAD") {
			t.Error("registry received credentials or a write", r.Method)
		}
		if r.URL.Path == "/v2/" {
			w.WriteHeader(200)
			return
		}
		requests.Add(1)
		if strings.Contains(r.URL.Path, "/source/") && status.Load() != 200 {
			w.WriteHeader(int(status.Load()))
			return
		}
		w.Header().Set("Docker-Content-Digest", filepath.Base(r.URL.Path))
		w.Header().Set("Content-Type", "application/vnd.oci.image.index.v1+json")
		w.Header().Set("Content-Length", "100")
	}))
	defer registry.Close()
	repository := strings.TrimPrefix(registry.URL, "http://")
	chart, err := archive(map[string][]byte{
		"oberth/Chart.yaml":  []byte("apiVersion: v2\nname: oberth\nversion: 1.2.3\nappVersion: v1.2.3\n"),
		"oberth/values.yaml": fmt.Appendf(nil, "imageRepository: %s/server\nsourcePluginImageRepository: %s/source\nimageDigest: sha256:%s\nsourcePluginImageDigest: sha256:%s\n", repository, repository, strings.Repeat("b", 64), strings.Repeat("c", 64)),
	}, false)
	if err != nil {
		t.Fatal(err)
	}
	chart, err = compressed(chart)
	if err != nil {
		t.Fatal(err)
	}
	files, public := signedDownloadFiles(t, map[string][]byte{"oberth.tgz": chart})
	tool := compiledReleaseTool(t, public)
	for _, code := range []int{200, 404, 401, 403} {
		t.Run(fmt.Sprint(code), func(t *testing.T) {
			status.Store(int32(code))
			requests.Store(0)
			checkImagePublication(t, tool, files, imagePublicationCase{repository, code})
			// 401 and 403 stop after one check; 404 also runs clean
			// verification and 200 also publishes through GitHub.
			if want := map[int]int32{200: 4, 404: 4}[code] + map[int]int32{401: 2, 403: 2}[code]; requests.Load() != want {
				t.Fatal("did not check both chart image digests", requests.Load())
			}
		})
	}
}

type imagePublicationCase struct {
	repository string
	code       int
}

// githubProxy stands between the tool and GitHub: it records each tunnel the
// tool asks for and joins it to target, or refuses it when target is empty,
// so no test reaches the network.
func githubProxy(t *testing.T, target string) (*httptest.Server, func() []string) {
	t.Helper()
	var mu sync.Mutex
	var tunnels []string
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		tunnels = append(tunnels, r.Method+" "+r.Host)
		mu.Unlock()
		if target == "" || r.Method != http.MethodConnect {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		tunnel(t, w, target)
	}))
	t.Cleanup(proxy.Close)
	return proxy, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(tunnels)
	}
}

func tunnel(t *testing.T, w http.ResponseWriter, target string) {
	server, err := net.Dial("tcp", target)
	if err != nil {
		t.Error(err)
		w.WriteHeader(http.StatusBadGateway)
		return
	}
	client, _, err := w.(http.Hijacker).Hijack()
	if err != nil {
		t.Error(err)
		_ = server.Close()
		return
	}
	if _, err := io.WriteString(client, "HTTP/1.1 200 Connection established\r\n\r\n"); err != nil {
		t.Error(err)
	}
	go func() { _, _ = io.Copy(server, client); _ = server.Close() }()
	_, _ = io.Copy(client, server)
	_ = client.Close()
}

// githubTLS serves the fake under GitHub's API and upload host names with a
// test certificate and returns its address and the certificate file.
func githubTLS(t *testing.T, fake *githubFake) (string, string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "api.github.com"},
		DNSNames: []string{"api.github.com", "uploads.github.com"}, NotBefore: time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC), NotAfter: time.Date(2100, 1, 1, 0, 0, 0, 0, time.UTC),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewUnstartedServer(http.HandlerFunc(fake.serve))
	server.TLS = &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}}
	server.StartTLS()
	t.Cleanup(server.Close)
	ca := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0600); err != nil {
		t.Fatal(err)
	}
	return server.Listener.Addr().String(), ca
}

// publishedThroughGitHub runs the built tool against the fake GitHub with
// stray files beside the signed ones: exactly the ten signed names reach the
// release, and a token file inside the release directory is refused.
func publishedThroughGitHub(t *testing.T, tool, dir, token string) {
	t.Helper()
	_, fake := githubFixture(t)
	fake.tags["v1.2.3"] = fakeCommit
	address, ca := githubTLS(t, fake)
	proxy, tunnels := githubProxy(t, address)
	for _, name := range []string{"notes.txt", "github-token"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("stray"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	run := func(token string) (string, error) {
		command := exec.CommandContext(t.Context(), tool, "publish", "--dir", dir, "--version", "v1.2.3", "--origin", releaseverify.Origin, "--token-file", token)
		command.Env = append(os.Environ(), "HTTPS_PROXY="+proxy.URL, "https_proxy="+proxy.URL, "NO_PROXY=", "no_proxy=", "SSL_CERT_FILE="+ca)
		output, err := command.CombinedOutput()
		return string(output), err
	}
	output, err := run(token)
	if err != nil || output != "Published v1.2.3: 10 files at "+releaseverify.Origin+"/v1.2.3.\n" {
		t.Fatal("publication through GitHub failed", err, output)
	}
	release := fake.find("v1.2.3", false)
	var names []string
	for _, asset := range release.Assets {
		names = append(names, asset.Name)
	}
	slices.Sort(names)
	want := append(publishedNames(), "SHA256SUMS", "SHA256SUMS.sigstore.json")
	slices.Sort(want)
	if !slices.Equal(names, want) || len(want) != 10 {
		t.Fatal("published assets are not exactly the signed names", names)
	}
	seen := len(tunnels())
	output, err = run(filepath.Join(dir, "github-token"))
	if err == nil || !strings.HasPrefix(output, tokenInside.Error()+"\n") || len(tunnels()) != seen {
		t.Fatal("a token file in the release directory was accepted", err, output)
	}
}

func checkImagePublication(t *testing.T, tool string, files map[string][]byte, tc imagePublicationCase) {
	code := tc.code
	t.Helper()
	proxy, tunnels := githubProxy(t, "")
	dir := t.TempDir()
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), body, 0600); err != nil {
			t.Fatal(err)
		}
	}
	makeImageLayouts(t, dir)
	credentials := t.TempDir()
	token := filepath.Join(credentials, "token")
	if err := os.WriteFile(token, []byte("fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	command := exec.CommandContext(t.Context(), tool, "publish", "--dir", dir, "--version", "v1.2.3", "--origin", releaseverify.Origin, "--token-file", token)
	command.Env = append(os.Environ(), "HTTPS_PROXY="+proxy.URL, "https_proxy="+proxy.URL, "NO_PROXY=", "no_proxy=")
	output, err := command.CombinedOutput()
	if code == 200 {
		if err == nil || !slices.Equal(tunnels(), []string{"CONNECT api.github.com:443"}) {
			t.Fatal("present images did not proceed to GitHub", err, string(output), tunnels())
		}
		publishedThroughGitHub(t, tool, dir, token)
		return
	}
	if err == nil || len(tunnels()) != 0 {
		t.Fatal("image refusal contacted GitHub or succeeded", err, tunnels())
	}
	assertImageRefusal(t, output, dir, tc)
	if code == 404 {
		verifyMissingImage(t, tool, dir, credentials)
	}
}

func assertImageRefusal(t *testing.T, output []byte, dir string, tc imagePublicationCase) {
	t.Helper()
	repository, code := tc.repository, tc.code
	want := imagePrivate
	if code == 404 {
		want = imageMissing
	}
	if !strings.HasPrefix(string(output), want.Error()+"\n"+want.Remedy()+"\n") {
		t.Fatal("image refusal lost guidance", string(output))
	}
	for _, detail := range []string{repository + "/source@sha256:" + strings.Repeat("c", 64), "go tool -modfile=hack/tools/crane.mod crane push " + shellQuote(filepath.Join(dir, "oberth-source-plugin.oci")) + " " + shellQuote(repository+"/source:v1.2.3")} {
		if !strings.Contains(string(output), detail) {
			t.Fatal("refusal omitted exact image or push", detail, string(output))
		}
	}
}

func makeImageLayouts(t *testing.T, dir string) {
	t.Helper()
	for _, image := range []string{"oberth", "oberth-source-plugin"} {
		if err := os.Mkdir(filepath.Join(dir, image+".oci"), 0700); err != nil {
			t.Fatal(err)
		}
	}
}
func verifyMissingImage(t *testing.T, tool, dir, credentials string) {
	t.Helper()

	verify := exec.CommandContext(t.Context(), tool, "verify", "--dir", dir, "--version", "v1.2.3", "--receipt", filepath.Join(credentials, "receipt"))
	output, err := verify.CombinedOutput()
	if err == nil || strings.Contains(string(output), "crane push") || !strings.Contains(string(output), "original build directory") {
		t.Fatal("clean verification suggested an unavailable layout", err, string(output))
	}
}
