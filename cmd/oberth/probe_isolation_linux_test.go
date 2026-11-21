//go:build linux

// These tests exercise the Linux server and Git receive path.
package main

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/oberthci/oberth/internal/api/fake"
)

func TestHealthProbesSurviveBothSaturatedTLSListeners(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, listeners, trace := probeServing(t)
		for _, address := range []string{s.c.config.Listen.HTTPS, s.c.config.Listen.Source} {
			for range 512 {
				peer, err := listeners[address].dial(t.Context(), "", "")
				if err != nil {
					t.Fatal(err)
				}
				defer peer.Close()
			}
			assertListenerSaturated(t, listeners[address])
		}
		tr := &http.Transport{DialContext: listeners[s.c.config.Listen.Health].dial}
		defer tr.CloseIdleConnections()
		client := &http.Client{Transport: tr, Timeout: time.Second}
		assertProbesAnswer(t, client)
		synctest.Wait()
		timer := time.NewTimer(10 * time.Second)
		<-timer.C
		synctest.Wait()
		if got := s.c.health.tlsHandshakeFailures.Load(); got != 1024 {
			t.Fatal("silent peers did not expire into the counter", got)
		}
		if strings.Contains(trace.String(), "handshake error") {
			t.Fatal("handshake flood was logged")
		}
	})
}

func assertProbesAnswer(t *testing.T, client *http.Client) {
	t.Helper()
	for _, path := range []string{"/livez", "/readyz"} {
		req, _ := http.NewRequestWithContext(t.Context(), "GET", "http://oberth.local"+path, nil)
		response, err := client.Do(req)
		if err != nil {
			t.Fatal("probe was held behind public connections", err)
		}
		_, _ = io.Copy(io.Discard, response.Body)
		_ = response.Body.Close()
		if response.StatusCode != http.StatusOK {
			t.Fatal(path, response.Status)
		}
	}
}

func probeServing(t *testing.T) (*serving, map[string]*memoryListener, *serverTrace) {
	t.Helper()
	clock := fake.NewClock()
	cfg, err := decodeConfig(validConfig())
	if err != nil {
		t.Fatal(err)
	}
	cert, key := authority(t, clock.Now())
	identity, err := newServerIdentity(cert, key, []string{"oberth.local"}, clock.Now)
	if err != nil {
		t.Fatal(err)
	}
	trace := &serverTrace{}
	h := &health{clock: serverClock{Now: clock.Now}, validLeaf: func() bool { return true }, started: true, success: clock.Now(), completed: clock.Now()}
	c := &composition{config: cfg, health: h, diagnostic: trace}
	listeners := map[string]*memoryListener{}
	deps := serverDependencies{listen: func(_ context.Context, _, address string) (net.Listener, error) {
		l := &memoryListener{incoming: make(chan net.Conn), closed: make(chan struct{})}
		listeners[address] = l
		return l, nil
	}}
	s := newServing(c, newLifecycle(t.Context(), nil), deps)
	t.Cleanup(func() {
		for _, server := range s.servers {
			_ = server.Close()
		}
		s.workers.Wait()
		s.cancelRequests()
	})
	if err = s.listen(t.Context(), identity); err != nil {
		t.Fatal(err)
	}
	return s, listeners, trace
}

func assertListenerSaturated(t *testing.T, l *memoryListener) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	result := make(chan error, 1)
	go func() {
		peer, err := l.dial(ctx, "", "")
		if peer != nil {
			_ = peer.Close()
		}
		result <- err
	}()
	synctest.Wait()
	select {
	case err := <-result:
		t.Fatal("listener accepted beyond its connection bound", err)
	default:
	}
	cancel()
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func TestProbeListenerHasItsOwnConnectionBound(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, listeners, _ := probeServing(t)
		l := listeners[s.c.config.Listen.Health]
		for range 8 {
			peer, err := l.dial(t.Context(), "", "")
			if err != nil {
				t.Fatal(err)
			}
			defer peer.Close()
		}
		assertListenerSaturated(t, l)
	})
}
