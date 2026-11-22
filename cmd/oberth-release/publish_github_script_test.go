//go:build linux

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
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// githubCurlHome serves the fake over TLS under GitHub's API and upload host
// names and returns a curl home that sends those names to it and trusts its
// certificate, so the hand tool runs unchanged and never reaches the network.
func githubCurlHome(t *testing.T, fake *githubFake) string {
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
	home := t.TempDir()
	ca := filepath.Join(home, "ca.pem")
	if err := os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0600); err != nil {
		t.Fatal(err)
	}
	address := server.Listener.Addr().String()
	config := fmt.Sprintf("connect-to = api.github.com:443:%s\nconnect-to = uploads.github.com:443:%s\ncacert = %s\nnoproxy = *\n", address, address, ca)
	if err := os.WriteFile(filepath.Join(home, ".curlrc"), []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	return home
}

type handTool struct {
	t          *testing.T
	fake       *githubFake
	home, dir  string
	tokenFile  string
	lastOutput string
}

func newHandTool(t *testing.T) *handTool {
	t.Helper()
	_, fake := githubFixture(t)
	h := &handTool{t: t, fake: fake, home: githubCurlHome(t, fake), dir: t.TempDir(), tokenFile: filepath.Join(t.TempDir(), "github")}
	for name, body := range map[string]string{"oberth-linux-amd64": "binary", "release.json": "record", "SHA256SUMS": "sums", "SHA256SUMS.sigstore.json": "bundle"} {
		if err := os.WriteFile(filepath.Join(h.dir, name), []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Join(h.dir, "oberth.oci"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(h.tokenFile, []byte("fixture\n"), 0600); err != nil {
		t.Fatal(err)
	}
	return h
}

func (h *handTool) run(args ...string) error {
	h.t.Helper()
	command := exec.CommandContext(h.t.Context(), "bash", append([]string{"../../hack/publish-github-release.sh"}, args...)...)
	command.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + h.home, "CURL_HOME=" + h.home, "TMPDIR=" + h.t.TempDir()}
	output, err := command.CombinedOutput()
	h.lastOutput = string(output)
	if strings.Contains(h.lastOutput, "fixture") {
		h.t.Fatal("the token reached the output")
	}
	return err
}

func TestHandPublicationDryRunWritesNothing(t *testing.T) {
	h := newHandTool(t)
	if err := h.run("--dry-run", "v2.0.0", h.dir, h.tokenFile); err != nil {
		t.Fatal(err, h.lastOutput)
	}
	for _, line := range []string{"Would create a draft release for v2.0.0.", "Would upload oberth-linux-amd64.", "Would upload SHA256SUMS.sigstore.json.", "Would publish v2.0.0 (latest: true)."} {
		if !strings.Contains(h.lastOutput, line+"\n") {
			t.Fatal("dry run omits", line, h.lastOutput)
		}
	}
	if len(h.fake.writes()) != 0 {
		t.Fatal("dry run wrote", h.fake.writes())
	}
}

func TestHandPublicationUploadsManifestLastAndIsIdempotent(t *testing.T) {
	h := newHandTool(t)
	if err := h.run("v2.0.0", h.dir, h.tokenFile); err != nil {
		t.Fatal(err, h.lastOutput)
	}
	upload := "POST " + fakeRepository + "/releases/1/assets?name="
	want := []string{"POST " + fakeRepository + "/releases", upload + "oberth-linux-amd64", upload + "release.json", upload + "SHA256SUMS", upload + "SHA256SUMS.sigstore.json", "PATCH " + fakeRepository + "/releases/1"}
	if !reflect.DeepEqual(h.fake.writes(), want) {
		t.Fatal("publication writes differ", h.fake.writes())
	}
	h.fake.locked(func() {
		release := h.fake.find("v2.0.0", false)
		if h.fake.latest != "v2.0.0" || release == nil || string(release.bytes["oberth-linux-amd64"]) != "binary" || len(release.bytes) != 4 {
			t.Fatal("published bytes or latest differ", h.fake.latest)
		}
	})
	h.rerunWritesNothing(len(want))
}

// rerunWritesNothing reruns a finished publication with the same bytes, then
// with changed bytes, and expects no further write either time.
func (h *handTool) rerunWritesNothing(writes int) {
	h.t.Helper()
	if err := h.run("v2.0.0", h.dir, h.tokenFile); err != nil || len(h.fake.writes()) != writes {
		h.t.Fatal("identical rerun wrote", err, h.fake.writes(), h.lastOutput)
	}
	if err := os.WriteFile(filepath.Join(h.dir, "oberth-linux-amd64"), []byte("other binary"), 0600); err != nil {
		h.t.Fatal(err)
	}
	if err := h.run("v2.0.0", h.dir, h.tokenFile); err == nil || !strings.Contains(h.lastOutput, "Published release bytes differ.") || len(h.fake.writes()) != writes {
		h.t.Fatal("changed bytes were accepted or written", err, h.lastOutput)
	}
}

func TestHandPublicationResumesADraftAndKeepsANewerLatest(t *testing.T) {
	h := newHandTool(t)
	var draft *fakeRelease
	h.fake.locked(func() {
		h.fake.add("v3.0.0", false)
		h.fake.latest = "v3.0.0"
		draft = h.fake.add("v2.0.0", true)
		draft.put("oberth-linux-amd64", []byte("binary"))
	})
	if err := h.run("v2.0.0", h.dir, h.tokenFile); err != nil {
		t.Fatal(err, h.lastOutput)
	}
	h.fake.locked(func() {
		if draft.Draft || len(draft.bytes) != 4 || h.fake.latest != "v3.0.0" || len(h.fake.releases) != 2 {
			t.Fatal("draft was not resumed or latest moved backwards", h.fake.latest)
		}
		if body := h.fake.bodies[len(h.fake.bodies)-1]; body["make_latest"] != "false" {
			t.Fatal("older release was offered as latest", body)
		}
	})
}

func TestHandPublicationRefusesBeforeAnyWrite(t *testing.T) {
	for _, tc := range []struct{ name, tag, want string }{
		{"missing tag", "v9.0.0", "Release tag is not on GitHub."},
		{"invalid tag", "v2.0", "Release tag is invalid; use vMAJOR.MINOR.PATCH."},
		{"empty token", "v2.0.0", "GitHub token file is missing or empty; pass the token file path."},
		{"unsigned directory", "v2.0.0", "Release directory lacks signed files; pass a signed release directory."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHandTool(t)
			switch tc.name {
			case "empty token":
				if err := os.WriteFile(h.tokenFile, []byte(" \n"), 0600); err != nil {
					t.Fatal(err)
				}
			case "unsigned directory":
				if err := os.WriteFile(filepath.Join(h.dir, "SHA256SUMS.sigstore.json"), nil, 0600); err != nil {
					t.Fatal(err)
				}
			}
			if err := h.run(tc.tag, h.dir, h.tokenFile); err == nil || !strings.HasPrefix(h.lastOutput, tc.want+"\n") {
				t.Fatal("refusal differs", err, h.lastOutput)
			}
			if len(h.fake.writes()) != 0 {
				t.Fatal("a refusal wrote", h.fake.writes())
			}
		})
	}
}
