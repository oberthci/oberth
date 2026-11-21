package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"time"
)

const probeConnections = 8

func probeServer(handler http.Handler) *http.Server {
	s := &http.Server{Handler: handler, ReadHeaderTimeout: time.Second, ReadTimeout: 2 * time.Second, WriteTimeout: 2 * time.Second, IdleTimeout: 5 * time.Second, MaxHeaderBytes: 1024}
	s.SetKeepAlivesEnabled(false)
	return s
}

func (h *health) probe(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/livez" && r.URL.Path != "/readyz" {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	h.writeProbe(w, r)
}

func (h *health) writeProbe(w http.ResponseWriter, r *http.Request) {
	err := h.ready(r.Context())
	if r.URL.Path == "/livez" {
		err = h.live(r.Context())
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if err != nil {
		w.WriteHeader(http.StatusServiceUnavailable)
		_ = json.NewEncoder(w).Encode(err)
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]bool{"live": true})
}

type listenerLog struct {
	health *health
	output io.Writer
}

func (l listenerLog) Write(p []byte) (int, error) {
	if bytes.Contains(p, []byte("http: TLS handshake error from ")) {
		l.health.tlsHandshakeFailures.Add(1)
		return len(p), nil
	}
	return l.output.Write(p)
}
