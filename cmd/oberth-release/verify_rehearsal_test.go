package main

import (
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/oberthci/oberth/internal/releaseverify"
)

func TestVerifyRegistryConnectionFailureNamesTheRegistry(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	repository := listener.Addr().String() + "/oberth"
	if err = listener.Close(); err != nil {
		t.Fatal(err)
	}
	err = checkPublishedImage(t.Context(), repository, "sha256:"+strings.Repeat("b", 64))
	var failure imageLookupError
	if !errors.As(err, &failure) || failure.Error() != registryUnavailable.Error() || !strings.Contains(failure.Detail(), listener.Addr().String()) || errors.Unwrap(failure) == nil {
		t.Fatal("registry connection failure lost its reason or detail", err)
	}
}

func TestVerifyAcceptsOnlyTheExpectedBuildLayoutDirectories(t *testing.T) {
	files, public := signedDownloads(t)
	tool := compiledReleaseTool(t, public)
	for _, kind := range []string{"published", "build", "unknown directory", "layout symlink", "layout file", "tampered"} {
		t.Run(kind, func(t *testing.T) {
			dir := t.TempDir()
			for name, body := range files {
				if err := os.WriteFile(filepath.Join(dir, name), body, 0600); err != nil {
					t.Fatal(err)
				}
			}
			addLayoutCase(t, dir, kind)
			command := exec.CommandContext(t.Context(), tool, "verify", "--dir", dir, "--version", "v1.2.3")
			output, err := command.CombinedOutput()
			if kind == "published" || kind == "build" {
				if err != nil || string(output) != "Release verified.\n" {
					t.Fatal("valid release lacks one success line", err, string(output))
				}
			} else if err == nil || (!strings.Contains(string(output), releaseverify.Inventory.Error()) && !strings.Contains(string(output), releaseverify.Digest.Error())) {
				t.Fatal("invalid build output was accepted", err, string(output))
			}
		})
	}
}

func addLayoutCase(t *testing.T, dir, kind string) {
	t.Helper()
	switch kind {
	case "build":
		for _, name := range []string{"oberth.oci", "oberth-source-plugin.oci"} {
			if err := os.Mkdir(filepath.Join(dir, name), 0700); err != nil {
				t.Fatal(err)
			}
		}
	case "unknown directory":
		if err := os.Mkdir(filepath.Join(dir, "extra.oci"), 0700); err != nil {
			t.Fatal(err)
		}
	case "layout symlink":
		if err := os.Symlink(t.TempDir(), filepath.Join(dir, "oberth.oci")); err != nil {
			t.Fatal(err)
		}
	case "layout file":
		if err := os.WriteFile(filepath.Join(dir, "oberth.oci"), []byte("extra"), 0600); err != nil {
			t.Fatal(err)
		}
	case "tampered":
		if err := os.WriteFile(filepath.Join(dir, "oberth-linux-amd64"), []byte("tampered"), 0600); err != nil {
			t.Fatal(err)
		}
	}
}

func TestRegistryRequiringReadCredentialsRefusesAnonymousPublication(t *testing.T) {
	var tokenRequests atomic.Int32
	var registry *httptest.Server
	registry = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			t.Error("anonymous read sent credentials")
		}
		if r.URL.Path == "/token" {
			tokenRequests.Add(1)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("WWW-Authenticate", `Bearer realm="`+registry.URL+`/token",service="registry"`)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer registry.Close()
	err := checkPublishedImage(t.Context(), strings.TrimPrefix(registry.URL, "http://")+"/oberth", "sha256:"+strings.Repeat("b", 64))
	if !errors.Is(err, imagePrivate) || tokenRequests.Load() == 0 {
		t.Fatal("private registry did not explain public-read requirement", err, tokenRequests.Load())
	}
}
