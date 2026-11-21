package main

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	artifact "github.com/argoproj/argo-workflows/v4/pkg/apiclient/artifact"
	"github.com/oberthci/oberth/internal/sourceplugin"
	"k8s.io/utils/clock"
)

// A closed Unix listener leaves an endpoint that genuinely refuses connections.
// Serving starts only after Load requests its first backoff, without a TCP port.
func restartPluginConfig(t *testing.T, handler http.Handler) sourceplugin.Config {
	t.Helper()
	socket := refusingSocket(t)
	cert, ca := testCertificate(t)
	server := &http.Server{Handler: handler, TLSConfig: &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}}
	var done chan error
	attempts, waits := 0, 0
	t.Cleanup(func() {
		_ = server.Close()
		if done != nil {
			if err := <-done; !errors.Is(err, http.ErrServerClosed) {
				t.Error(err)
			}
			if attempts != 2 || waits != 1 {
				t.Errorf("restart attempts=%d waits=%d", attempts, waits)
			}
		}
	})
	return sourceplugin.Config{Endpoint: "https://source.test", ServerName: "source.test", CAPEM: ca, TokenFile: pluginIdentity(t), Now: clock.RealClock{}.Now,
		Jitter: func(d time.Duration) time.Duration { return d },
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			attempts++
			conn, err := (&net.Dialer{}).DialContext(ctx, "unix", socket)
			if attempts == 1 && !errors.Is(err, syscall.ECONNREFUSED) {
				t.Errorf("initial connection did not refuse: %v", err)
			}
			return conn, err
		},
		Wait: func(ctx context.Context, delay time.Duration) error {
			waits++
			if delay != time.Second {
				t.Errorf("first backoff=%s", delay)
			}
			if err := os.Remove(socket); err != nil {
				return err
			}
			var config net.ListenConfig
			listener, err := config.Listen(ctx, "unix", socket)
			if err != nil {
				return err
			}
			done = make(chan error, 1)
			go func() { done <- server.Serve(tls.NewListener(listener, server.TLSConfig)) }()
			return nil
		}}
}

func refusingSocket(t *testing.T) string {
	t.Helper()
	// Keep the socket path below the platform limit even with a long test name.
	dir, err := os.MkdirTemp("", "c6-restart-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(dir); err != nil {
			t.Error(err)
		}
	})
	socket := filepath.Join(dir, "source.sock")
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Net: "unix", Name: socket})
	if err != nil {
		t.Fatal(err)
	}
	listener.SetUnlinkOnClose(false)
	if err = listener.Close(); err != nil {
		t.Fatal(err)
	}
	return socket
}

func TestUnreachableServerRetriesUntilStepContextEnds(t *testing.T) {
	cfg := restartPluginConfig(t, nil)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var delays []time.Duration
	cfg.Wait = func(ctx context.Context, delay time.Duration) error {
		delays = append(delays, delay)
		if len(delays) == 5 {
			cancel()
			return context.Cause(ctx)
		}
		return nil
	}
	client, err := sourceplugin.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	path := filepath.Join(t.TempDir(), "source.tgz")
	input := &artifact.Artifact{Plugin: &artifact.PluginArtifact{Name: "oberth", Key: "run-1", Configuration: `{"version":1,"kind":"main"}`}}
	if err = client.Load(ctx, input, path); err == nil || !errors.Is(context.Cause(ctx), context.Canceled) {
		t.Fatalf("Load ended before step cancellation: %v", err)
	}
	if len(delays) != 5 {
		t.Fatal(delays)
	}
	for i, delay := range delays {
		want := 2 * time.Second
		if i == 0 {
			want = time.Second
		}
		if delay != want {
			t.Errorf("backoff %d=%s want %s", i, delay, want)
		}
	}
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil || len(entries) != 0 {
		t.Fatalf("unreachable Load left scratch: %v %v", entries, err)
	}
}
