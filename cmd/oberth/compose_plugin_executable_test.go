package main

import (
	"context"
	"encoding/pem"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/sourceplugin"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestSourcePluginExecutableRefusesAbsentMountedToken(t *testing.T) {
	mounted := sourceplugin.Environment().TokenFile
	if _, err := os.Stat(mounted); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("executable refusal fixture requires the native mounted token to be absent", err)
	}
	var requests atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { requests.Add(1); w.WriteHeader(500) }))
	defer server.Close()
	ca := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})
	trace := &serverTrace{ready: make(chan struct{})}
	socket := pluginSocket(t)
	ctx, cancel := context.WithCancel(t.Context())
	process := exec.CommandContext(ctx, buildSourcePlugin(t), socket)
	process.WaitDelay = 5 * time.Second
	process.Env = []string{"OBERTH_SOURCE_ENDPOINT=" + server.URL, "OBERTH_SOURCE_SERVER_NAME=example.com", "OBERTH_SOURCE_CA_PEM=" + string(ca)}
	process.Stdout, process.Stderr = trace, trace
	if err := process.Start(); err != nil {
		cancel()
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- process.Wait() }()
	defer joinPlugin(t, process, done, cancel)
	destination := filepath.Join(t.TempDir(), "source")
	response, err := loadPluginArtifact(t, socket, pluginRequest{Locator: "absent-token", Destination: destination})
	if status.Code(err) != codes.FailedPrecondition || !strings.Contains(err.Error(), mounted) || response.GetSuccess() {
		t.Fatalf("mounted-token refusal was not clean or named: %v; %s", err, trace.String())
	}
	if requests.Load() != 0 {
		t.Fatal("plugin contacted source without its mounted token")
	}
	if _, err = os.Stat(destination); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("refused plugin published a source artifact", err)
	}
}

func buildSourcePlugin(t *testing.T) string {
	t.Helper()
	binary := filepath.Join(t.TempDir(), "oberth-source-plugin")
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	build := exec.CommandContext(ctx, "go", "build", "-p=1", "-o", binary, "../oberth-source-plugin")
	build.WaitDelay = 5 * time.Second
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build native source plugin: %v: %s", err, out)
	}
	return binary
}
