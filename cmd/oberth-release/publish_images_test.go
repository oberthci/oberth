//go:build linux

// These tests run the built release tool against a local registry.
package main

import (
	"fmt"
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
			want := int32(2)
			if code == 404 {
				want = 4
			}
			if requests.Load() != want {
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
// tool asks for and refuses it, so no test reaches the network.
func githubProxy(t *testing.T) (*httptest.Server, func() []string) {
	t.Helper()
	var mu sync.Mutex
	var tunnels []string
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		tunnels = append(tunnels, r.Method+" "+r.Host)
		mu.Unlock()
		w.WriteHeader(http.StatusForbidden)
	}))
	t.Cleanup(proxy.Close)
	return proxy, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(tunnels)
	}
}

func checkImagePublication(t *testing.T, tool string, files map[string][]byte, tc imagePublicationCase) {
	code := tc.code
	t.Helper()
	proxy, tunnels := githubProxy(t)
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
