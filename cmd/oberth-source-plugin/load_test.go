package main

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	artifact "github.com/argoproj/argo-workflows/v4/pkg/apiclient/artifact"
	"github.com/oberthci/oberth/internal/gitcache/gittest"
	"github.com/oberthci/oberth/internal/sourcedelivery"
	"github.com/oberthci/oberth/internal/sourceplugin"
	"github.com/oberthci/oberth/internal/sshserver/sshtest"
	"k8s.io/utils/clock"
)

func TestLoadRealGitArchiveThroughPinnedHTTPSPlugin(t *testing.T) {
	checkPluginLoad(t, false)
}
func TestRestartWaitDeliversRealGitWithoutNativeRetry(t *testing.T) {
	checkPluginLoad(t, true)
}
func checkPluginLoad(t *testing.T, restart bool) {
	f := gittest.New(t)
	cache := f.Cache(t, nil)
	binding := f.Binding()
	p := prepared(t, binding)
	delivery := sourcedelivery.New(sourcedelivery.Config{Now: clock.RealClock{}.Now, SourceClientBurst: 30,
		Authenticate: func(_ context.Context, token string) (sourcedelivery.Caller, error) {
			if token != "bound-token" {
				return sourcedelivery.Caller{}, &sourcedelivery.Failure{Code: "unauthenticated"}
			}
			return sourcedelivery.Caller{Namespace: "runs", Workflow: "wf", UID: "uid"}, nil
		},
		Run: func(context.Context, string) (sourcedelivery.Run, error) {
			return sourcedelivery.Run{Admission: p.View(), Namespace: "runs", Workflow: "wf", UID: "uid"}, nil
		}, Stage: cache.Stage})
	var cfg sourceplugin.Config
	if restart {
		cfg = restartPluginConfig(t, delivery)
	} else {
		cfg = pluginConfig(t, delivery)
	}
	client, err := sourceplugin.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	descriptor, err := json.Marshal(binding.Descriptor)
	if err != nil {
		t.Fatal(err)
	}
	input := &artifact.Artifact{Plugin: &artifact.PluginArtifact{Name: "oberth", Key: "run-1", Configuration: string(descriptor)}}
	scratch := filepath.Join(f.Root, "helper")
	if err = os.Mkdir(scratch, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(scratch, "source.tgz")
	if err = client.Load(t.Context(), input, path); err != nil {
		t.Fatal(err)
	}
	checkSourceArchive(t, path)
	entries, err := os.ReadDir(scratch)
	if err != nil || len(entries) != 1 {
		t.Fatalf("staging residue: %v %v", entries, err)
	}
}
func checkSourceArchive(t *testing.T, path string) {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = gz.Close() }()
	tr := tar.NewReader(gz)
	found := false
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if h.Mode&0222 != 0 {
			t.Fatalf("writable source entry %s", h.Name)
		}
		if h.Name == "file.txt" {
			data, err := io.ReadAll(tr)
			if err != nil || string(data) != "exact source\n" {
				t.Fatalf("source %q %v", data, err)
			}
			found = true
		}
	}
	if !found {
		t.Fatal("source did not reach plugin")
	}
}
func pluginConfig(t *testing.T, handler http.Handler) sourceplugin.Config {
	t.Helper()
	cert, ca := testCertificate(t)
	left, right := sshtest.Pipe()
	listener := sshtest.Listener(right)
	server := &http.Server{Handler: handler, TLSConfig: &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}}
	done := make(chan error, 1)
	go func() { done <- server.Serve(tls.NewListener(listener, server.TLSConfig)) }()
	t.Cleanup(func() {
		_ = server.Close()
		_ = left.Close()
		_ = right.Close()
		if err := <-done; err != nil && !errors.Is(err, http.ErrServerClosed) {
			t.Error(err)
		}
	})
	return sourceplugin.Config{Endpoint: "https://source.test", ServerName: "source.test", CAPEM: ca, TokenFile: pluginIdentity(t), Now: clock.RealClock{}.Now, DialContext: func(context.Context, string, string) (net.Conn, error) { return left, nil }}
}
func pluginIdentity(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(path, []byte("bound-token"), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func testCertificate(t *testing.T) (tls.Certificate, []byte) {
	t.Helper()
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	cert := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "source.test"}, DNSNames: []string{"source.test"}, IsCA: true, BasicConstraintsValid: true, NotBefore: time.Unix(0, 0), NotAfter: time.Date(2100, 1, 1, 0, 0, 0, 0, time.UTC), KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, cert, cert, public, private)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: private}, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}
