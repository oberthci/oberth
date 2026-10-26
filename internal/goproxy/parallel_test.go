package goproxy

import (
	"archive/tar"
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type parallelCache struct{ active, peak, archives atomic.Int32 }

func (*parallelCache) ListTags(context.Context, string) ([]string, error) {
	return []string{"v1.0.0"}, nil
}
func (*parallelCache) TagTime(context.Context, string, string) (time.Time, error) {
	return time.Date(2026, 10, 26, 0, 0, 0, 0, time.UTC), nil
}
func (*parallelCache) ShowFile(_ context.Context, input, _, _ string) ([]byte, error) {
	return []byte("module go.example.test/" + input + "\n\ngo 1.26\n"), nil
}
func (cache *parallelCache) ArchiveTar(ctx context.Context, input, _ string, output io.Writer) error {
	active := cache.active.Add(1)
	defer cache.active.Add(-1)
	for previous := cache.peak.Load(); active > previous; previous = cache.peak.Load() {
		if cache.peak.CompareAndSwap(previous, active) {
			break
		}
	}
	cache.archives.Add(1)
	// Keep concurrent archive reads observable without a wall-clock load test.
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(time.Millisecond):
	}
	writer := tar.NewWriter(output)
	for name, body := range map[string]string{
		"go.mod":    "module go.example.test/" + input + "\n\ngo 1.26\n",
		"module.go": "package fixture\n//" + strings.Repeat("payload ", 8192) + "\n",
	} {
		if err := writer.WriteHeader(&tar.Header{Name: name, Mode: 0644, Size: int64(len(body))}); err != nil {
			return err
		}
		if _, err := io.WriteString(writer, body); err != nil {
			return err
		}
	}
	return writer.Close()
}

// Concurrency regression for the Go resolver, TLS accept loop and canonical
// private zip handler with four cold caches and a 64-module dependency graph.
// It uses no external network and does not reproduce #1204's Kubernetes
// readiness/Service routing failure.
func TestParallelGoModDownloads(t *testing.T) {
	goBinary, err := exec.LookPath("go")
	if err != nil {
		t.Skipf("Go toolchain unavailable: %v", err)
	}
	const modules, clients = 64, 4
	ca, _, certPEM, keyPEM := selfSignedCA(t)
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatal(err)
	}
	cache := &parallelCache{}
	mapping := map[string]string{}
	var root strings.Builder
	root.WriteString("module fixture.invalid/root\n\ngo 1.26\n\nrequire (\n")
	for index := range modules {
		name := fmt.Sprintf("module%d", index)
		mapping[name] = name
		fmt.Fprintf(&root, " go.example.test/%s v1.0.0\n", name)
	}
	root.WriteString(")\n")
	server := &http.Server{
		Handler:           NewHandler(cache, "go.example.test", mapping),
		ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 30 * time.Second,
		IdleTimeout: 30 * time.Second, WriteTimeout: 2 * time.Minute,
		TLSConfig: &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{cert}},
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	served := make(chan error, 1)
	go func() { served <- server.Serve(tls.NewListener(listener, server.TLSConfig)) }()
	t.Cleanup(func() { _ = server.Close(); <-served })
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	var workers sync.WaitGroup
	for range clients {
		dir := t.TempDir()
		for name, body := range map[string][]byte{"go.mod": []byte(root.String()), "ca.pem": ca} {
			if err := os.WriteFile(filepath.Join(dir, name), body, 0600); err != nil {
				t.Fatal(err)
			}
		}
		workers.Go(func() {
			command := exec.CommandContext(ctx, goBinary, "mod", "download")
			command.Dir = dir
			command.Env = []string{
				"PATH=" + filepath.Dir(goBinary), "HOME=" + dir, "GOENV=off", "GOWORK=off", "GOTOOLCHAIN=local",
				"GOMODCACHE=" + filepath.Join(dir, "modules"), "GOCACHE=" + filepath.Join(dir, "build"),
				"GOPATH=" + filepath.Join(dir, "gopath"), "GOPROXY=https://" + listener.Addr().String(),
				"GOFLAGS=-modcacherw", "GONOPROXY=none", "GONOSUMDB=go.example.test", "GOSUMDB=off", "GOVCS=*:off", "GOMAXPROCS=4",
				"SSL_CERT_FILE=" + filepath.Join(dir, "ca.pem"), "SSL_CERT_DIR=" + dir, "GOTELEMETRY=off",
			}
			if output, err := command.CombinedOutput(); err != nil {
				t.Errorf("parallel download: %v: %s", err, output)
			}
		})
	}
	workers.Wait()
	if cache.archives.Load() != modules*clients || cache.peak.Load() < 2 {
		t.Fatalf("archives=%d want=%d peak_concurrency=%d", cache.archives.Load(), modules*clients, cache.peak.Load())
	}
	t.Logf("clients=%d modules=%d archives=%d peak_concurrency=%d", clients, modules, cache.archives.Load(), cache.peak.Load())
}
