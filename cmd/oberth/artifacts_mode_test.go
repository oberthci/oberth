package main

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestArtifactsReadsTheServerWhenOneIsConfigured(t *testing.T) {
	payload := `{"run_id":"run-abc","artifacts":[{"name":"surefire/TEST.xml","size":42,"modified":"2026-08-24T10:00:00Z"}]}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/artifacts") {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		_, _ = w.Write([]byte(payload))
	}))
	defer server.Close()
	configure(t, server)

	var out bytes.Buffer
	if err := runArtifacts(context.Background(), []string{"run-abc"}, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "surefire/TEST.xml") {
		t.Fatalf("output:\n%s", out.String())
	}
}

func TestArtifactsReadsTheLocalStoreWhenNoServerIsConfigured(t *testing.T) {
	t.Setenv("OBERTH_BASE_URL", "")
	t.Setenv("OBERTH_TOKEN", "")
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "artifacts", "run-local"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "artifacts", "run-local", "report.txt"), []byte("local body\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	if err := runArtifacts(context.Background(), []string{"--data-root", root, "run-local"}, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "report.txt") {
		t.Fatalf("the local store was not read:\n%s", out.String())
	}

	var body bytes.Buffer
	if err := runArtifacts(context.Background(), []string{"--data-root", root, "run-local", "report.txt"}, &body); err != nil {
		t.Fatal(err)
	}
	if body.String() != "local body\n" {
		t.Fatalf("local read returned %q", body.String())
	}
}

func TestArtifactsModeLineNeverReachesStdout(t *testing.T) {
	t.Setenv("OBERTH_BASE_URL", "")
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "artifacts"), 0o750); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := runArtifacts(context.Background(), []string{"--data-root", root, "run-none"}, &out); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "reading:") {
		t.Fatalf("the mode line contaminated stdout:\n%s", out.String())
	}
}

// --- #242: --data-root overrides OBERTH_BASE_URL ---

func TestArtifactsExplicitDataRootForcesLocalMode(t *testing.T) {
	// Both a remote server and a local store are configured.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"run_id":"r","artifacts":[{"name":"remote.txt","size":1,"modified":"2026-08-25T00:00:00Z"}]}`))
	}))
	defer server.Close()
	configure(t, server) // sets OBERTH_BASE_URL

	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "artifacts", "run-local"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "artifacts", "run-local", "local.txt"), []byte("local\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	// Without --data-root, reads the remote server.
	var remoteOut bytes.Buffer
	if err := runArtifacts(context.Background(), []string{"run-abc"}, &remoteOut); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(remoteOut.String(), "remote.txt") {
		t.Fatalf("without --data-root, should read the remote:\n%s", remoteOut.String())
	}

	// With explicit --data-root, forces the local store.
	var localOut bytes.Buffer
	if err := runArtifacts(context.Background(), []string{"--data-root", root, "run-local"}, &localOut); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(localOut.String(), "local.txt") {
		t.Fatalf("explicit --data-root should force local mode:\n%s", localOut.String())
	}
}
