package trustedharness

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"
)

func fetchTLSServer(t *testing.T, version uint16, handler http.HandlerFunc) *httptest.Server {
	t.Helper()
	server := httptest.NewUnstartedServer(handler)
	server.TLS = &tls.Config{MinVersion: version, MaxVersion: version}
	// Refused handshakes are the expected result for obsolete TLS fixtures.
	server.Config.ErrorLog = log.New(io.Discard, "", 0)
	server.StartTLS()
	t.Cleanup(server.Close)
	return server
}

func fetchServerRoots(server *httptest.Server) *x509.CertPool {
	roots := x509.NewCertPool()
	roots.AddCert(server.Certificate())
	return roots
}

func TestFetchRefusesHTTPSRedirects(t *testing.T) {
	t.Parallel()
	body := []byte("pinned suite bundle")
	var destinationRequests atomic.Int32
	destination := fetchTLSServer(t, tls.VersionTLS13, func(w http.ResponseWriter, _ *http.Request) {
		destinationRequests.Add(1)
		_, _ = w.Write(body)
	})
	for _, status := range []int{http.StatusMovedPermanently, http.StatusFound, http.StatusSeeOther, http.StatusTemporaryRedirect, http.StatusPermanentRedirect} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			var sourceRequests atomic.Int32
			source := fetchTLSServer(t, tls.VersionTLS13, func(w http.ResponseWriter, r *http.Request) {
				sourceRequests.Add(1)
				http.Redirect(w, r, destination.URL+"/bundle", status)
			})
			roots := fetchServerRoots(source)
			roots.AddCert(destination.Certificate())
			got, err := fetch(context.Background(), source.URL+"/bundle", digest(body), 1024, roots)
			if err == nil || got != nil {
				t.Fatalf("redirect accepted: body=%q, err=%v", got, err)
			}
			if sourceRequests.Load() != 1 || destinationRequests.Load() != 0 {
				t.Fatalf("redirect request counts: source=%d destination=%d", sourceRequests.Load(), destinationRequests.Load())
			}
		})
	}
}

func TestFetchRequiresTLS13(t *testing.T) {
	t.Parallel()
	for _, version := range []uint16{tls.VersionTLS12, tls.VersionTLS13} {
		t.Run(tls.VersionName(version), func(t *testing.T) {
			body := []byte("pinned suite bundle")
			var negotiated atomic.Uint32
			server := fetchTLSServer(t, version, func(w http.ResponseWriter, r *http.Request) {
				negotiated.Store(uint32(r.TLS.Version))
				_, _ = w.Write(body)
			})
			got, err := fetch(context.Background(), server.URL+"/bundle", digest(body), 1024, fetchServerRoots(server))
			if version == tls.VersionTLS13 {
				if err != nil || !bytes.Equal(got, body) || negotiated.Load() != uint32(tls.VersionTLS13) {
					t.Fatalf("TLS 1.3 fetch: body=%q, err=%v, negotiated=%x", got, err, negotiated.Load())
				}
			} else if err == nil || got != nil || negotiated.Load() != 0 {
				t.Fatalf("obsolete TLS reached handler: body=%q, err=%v, negotiated=%x", got, err, negotiated.Load())
			}
		})
	}
}

func TestFetchVerifiesHTTPSCertificate(t *testing.T) {
	t.Parallel()
	body := []byte("pinned suite bundle")
	var requests atomic.Int32
	server := fetchTLSServer(t, tls.VersionTLS13, func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		_, _ = w.Write(body)
	})
	got, err := fetch(context.Background(), server.URL+"/bundle", digest(body), 1024, x509.NewCertPool())
	if err == nil || got != nil || requests.Load() != 0 {
		t.Fatalf("untrusted certificate accepted: body=%q, err=%v, requests=%d", got, err, requests.Load())
	}
}
