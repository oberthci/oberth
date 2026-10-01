package vmnodebridge

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"time"

	"github.com/oberthci/oberth/internal/vmrunner"
)

const AttachPath = "/v1/conductor/attach"
const maxOpenFrame = 24 << 10
const maxNodeClaims = 4096

// Server exposes one fixed operation over HTTP/2 mTLS. Its CRI client and
// streaming URL remain node-local. Activated must remain false until the
// installed runtime and protected streaming endpoint have been qualified.
type Server struct {
	Runtime          *Runtime
	Streamer         RuntimeStreamer
	StreamTLS        StreamTLS
	AllowedClientURI string
	Activated        bool
	mu               sync.Mutex
	used             map[string]time.Time
}

func ServerTLSConfig(cert tls.Certificate, clientCA []byte, nodeName string) (*tls.Config, error) {
	pool := x509.NewCertPool()
	if len(cert.Certificate) == 0 || !pool.AppendCertsFromPEM(clientCA) {
		return nil, errRuntimeIdentity
	}
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil || nodeName == "" || leaf.VerifyHostname(nodeName) != nil {
		return nil, errRuntimeIdentity
	}
	return &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{cert},
		ClientAuth: tls.RequireAndVerifyClientCert, ClientCAs: pool, NextProtos: []string{"h2"}}, nil
}

func (server *Server) authorized(r *http.Request) bool {
	if server == nil || !server.Activated || server.Runtime == nil || server.Streamer == nil ||
		server.AllowedClientURI == "" || r.TLS == nil || r.TLS.Version < tls.VersionTLS13 || len(r.TLS.VerifiedChains) == 0 ||
		len(r.TLS.PeerCertificates) == 0 || len(r.TLS.PeerCertificates[0].URIs) != 1 || r.ProtoMajor != 2 {
		return false
	}
	want, err := url.Parse(server.AllowedClientURI)
	return err == nil && want.Scheme != "" && r.TLS.PeerCertificates[0].URIs[0].String() == want.String()
}

func (server *Server) spend(request AttachRequest) bool {
	key := vmrunner.PilotIdentity(request.Plan) + ":" + request.Claim.AttemptIdentity + ":" + request.Claim.Challenge
	server.mu.Lock()
	defer server.mu.Unlock()
	if server.used == nil {
		server.used = make(map[string]time.Time)
	}
	now := time.Now()
	for prior, deadline := range server.used {
		if !now.Before(deadline) {
			delete(server.used, prior)
		}
	}
	if len(server.used) >= maxNodeClaims {
		return false
	}
	if _, exists := server.used[key]; exists {
		return false
	}
	server.used[key] = request.CreatedAt.Add(request.Plan.Spec.Deadline)
	return true
}

func (server *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != AttachPath || r.URL.RawQuery != "" || r.Method != http.MethodPost ||
		r.Header.Get("Content-Type") != "application/octet-stream" || !server.authorized(r) {
		http.Error(w, "attach unavailable", http.StatusForbidden)
		return
	}
	defer func() { _ = r.Body.Close() }()
	framed := bufio.NewReaderSize(r.Body, maxOpenFrame)
	line, err := framed.ReadSlice('\n')
	if err != nil || len(line) < 2 || len(line) >= maxOpenFrame {
		http.Error(w, "invalid attach request", http.StatusBadRequest)
		return
	}
	var request AttachRequest
	body := line[:len(line)-1]
	if json.Unmarshal(body, &request) != nil {
		http.Error(w, "invalid attach request", http.StatusBadRequest)
		return
	}
	canonical, err := json.Marshal(request)
	if err != nil || !bytes.Equal(body, canonical) || request.validate(server.Runtime.NodeName, time.Now()) != nil {
		http.Error(w, "invalid attach request", http.StatusBadRequest)
		return
	}
	if !server.spend(request) {
		http.Error(w, "attach already spent", http.StatusConflict)
		return
	}
	ctx, cancel := context.WithDeadline(r.Context(), request.CreatedAt.Add(request.Plan.Spec.Deadline))
	defer cancel()
	runtimeURL, observed, err := server.Runtime.Attach(ctx, request)
	if err != nil || server.StreamTLS.validateURL(runtimeURL) != nil {
		http.Error(w, "runtime attach unavailable", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("X-Oberth-Runtime-Started-At", strconv.FormatInt(observed.RuntimeStartedAt.UnixNano(), 10))
	w.Header().Set("Trailer", "X-Oberth-Attach-Result")
	w.WriteHeader(http.StatusOK)
	if err := http.NewResponseController(w).Flush(); err != nil {
		return
	}
	stdout := &boundedWriter{target: flushWriter{writer: w}, left: maxOutputBytes, cancel: cancel}
	stderr := &boundedWriter{target: io.Discard, left: maxStderrBytes, cancel: cancel}
	stdin := &boundedReader{source: framed, left: maxInputBytes, cancel: cancel}
	err = server.Streamer.Stream(ctx, runtimeURL, stdin, stdout, stderr)
	if err != nil || ctx.Err() != nil {
		w.Header().Set("X-Oberth-Attach-Result", "error")
		return
	}
	w.Header().Set("X-Oberth-Attach-Result", "ok")
}

type flushWriter struct{ writer http.ResponseWriter }

func (writer flushWriter) Write(body []byte) (int, error) {
	n, err := writer.writer.Write(body)
	if err != nil || n != len(body) {
		return n, errRuntimeStream
	}
	if err := http.NewResponseController(writer.writer).Flush(); err != nil {
		return n, errRuntimeStream
	}
	return n, nil
}
