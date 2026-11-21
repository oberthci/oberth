package main

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/api/fake"
)

func TestProbeListenerReportsHealthWithoutOtherRoutes(t *testing.T) {
	clock := fake.NewClock()
	h := &health{clock: serverClock{Now: clock.Now}, validLeaf: func() bool { return true }, completed: clock.Now(), success: clock.Now()}

	for _, tc := range []struct {
		started bool
		path    string
		status  int
	}{
		{false, "/livez", 200}, {false, "/readyz", 503}, {true, "/livez", 200}, {true, "/readyz", 200},
		{true, "/mcp", 404}, {true, "/metrics", 404}, {true, "/api/runs", 404},
	} {
		h.setStarted(tc.started)
		w := httptest.NewRecorder()
		h.probe(w, httptest.NewRequest(http.MethodGet, tc.path, nil))
		if w.Code != tc.status {
			t.Fatal(tc, w.Code)
		}
	}

	h.stop()
	w := httptest.NewRecorder()
	h.probe(w, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if w.Code != http.StatusServiceUnavailable {
		t.Fatal("stopping server is ready")
	}
	w = httptest.NewRecorder()
	h.probe(w, httptest.NewRequest(http.MethodPost, "/livez", nil))
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatal("probe accepted a mutation")
	}
}

func TestProbeListenerBounds(t *testing.T) {
	s := probeServer(http.NotFoundHandler())
	if s.ReadHeaderTimeout != time.Second || s.ReadTimeout != 2*time.Second || s.WriteTimeout != 2*time.Second || s.IdleTimeout != 5*time.Second || s.MaxHeaderBytes != 1024 {
		t.Fatal("probe I/O bounds changed")
	}
}

func TestListenerLogCountsHandshakeFailuresAndKeepsOtherErrors(t *testing.T) {
	var output bytes.Buffer
	h := &health{}
	l := listenerLog{h, &output}
	for range 512 {
		p := []byte("http: TLS handshake error from peer: i/o timeout\n")
		if n, err := l.Write(p); n != len(p) || err != nil {
			t.Fatal(n, err)
		}
	}
	if output.Len() != 0 || !strings.Contains(h.metrics(context.Background()), "oberth_tls_handshake_failures_total 512\n") {
		t.Fatal("handshake flood was not counted", output.String())
	}
	other := "http: panic serving peer: handler failed\n"
	if _, err := l.Write([]byte(other)); err != nil {
		t.Fatal(err)
	}
	if output.String() != other {
		t.Fatal("other server diagnostics were hidden", output.String())
	}
}
