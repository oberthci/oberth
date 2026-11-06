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

	"github.com/oberthci/oberth/internal/testtimeout"

	"github.com/oberthci/oberth/internal/gitcache"
)

type parallelCache struct {
	active, peak, archives atomic.Int32
	overlap                chan struct{}
}

func (*parallelCache) ListTags(context.Context, string) ([]string, error) {
	return []string{"v1.0.0"}, nil
}
func (*parallelCache) ModuleTagSnapshot(context.Context, string, string) (string, time.Time, error) {
	return artifactOID, time.Date(2026, 10, 26, 0, 0, 0, 0, time.UTC), nil
}
func (*parallelCache) ModuleOriginURL(context.Context, string) (string, error) {
	return artifactOriginURL, nil
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
	arrival := cache.archives.Add(1)
	// The first archive cannot finish before another request is active.
	// A serial implementation fails with a bounded diagnostic, not a flaky peak.
	if arrival == 2 {
		close(cache.overlap)
	}
	if arrival <= 2 {
		timer := time.NewTimer(testtimeout.Start(10 * time.Second))
		defer timer.Stop()
		select {
		case <-cache.overlap:
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
			return fmt.Errorf("archive overlap barrier: second request did not arrive")
		}
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
	cache := &parallelCache{overlap: make(chan struct{})}
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
		Handler:           cachedTestHandler(t, cache, "go.example.test", mapping),
		ReadHeaderTimeout: testtimeout.Start(10 * time.Second), ReadTimeout: testtimeout.Start(30 * time.Second),
		IdleTimeout: testtimeout.Start(30 * time.Second), WriteTimeout: testtimeout.Start(2 * time.Minute),
		TLSConfig: &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{cert}},
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	served := make(chan error, 1)
	go func() { served <- server.Serve(tls.NewListener(listener, server.TLSConfig)) }()
	t.Cleanup(func() { _ = server.Close(); <-served })
	ctx, cancel := context.WithTimeout(t.Context(), testtimeout.Start(time.Minute))
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
	// Every client shares the one immutable version bundle for each module.
	if cache.archives.Load() != modules || cache.peak.Load() < 2 {
		t.Fatalf("archives=%d want=%d peak_concurrency=%d", cache.archives.Load(), modules, cache.peak.Load())
	}
	t.Logf("clients=%d modules=%d archives=%d peak_concurrency=%d", clients, modules, cache.archives.Load(), cache.peak.Load())
}

func (g *parallelCache) ModuleVersionSnapshot(ctx context.Context, repo, _, subdir, version string, _ bool) (gitcache.ModuleSnapshot, error) {
	tag := version
	if subdir != "" {
		tag = subdir + "/" + version
	}
	source, stamp, err := g.ModuleTagSnapshot(ctx, repo, tag)
	return gitcache.ModuleSnapshot{Commit: source, Time: stamp, Ref: "refs/tags/" + tag, URL: artifactOriginURL}, err
}
