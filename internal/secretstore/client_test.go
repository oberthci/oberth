package secretstore

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"io"
	"math/big"
	"net/http"
	"strings"
	"testing"
	"time"
)

type roundTrip func(*http.Request) (*http.Response, error)

func (f roundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// Covers: REQ-C9-027
func TestPrivateBoundedSecretDeliveryContract(t *testing.T) {
	calls := 0
	c, err := New(Connection{URL: "https://store.oberth.svc", Transport: roundTrip(func(r *http.Request) (*http.Response, error) {
		if err := r.Context().Err(); err != nil {
			return nil, err
		}
		calls++
		return &http.Response{StatusCode: 302, Header: http.Header{"Location": []string{"https://other.oberth.svc"}}, Body: io.NopCloser(strings.NewReader("do not expose this response"))}, nil
	})})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	status, err := c.JSON(ctx, Request{Method: "GET", Path: "/value", Token: []byte("private")}, nil)
	if status != 302 || err == nil || strings.Contains(err.Error(), "do not expose") {
		t.Fatal("redirect/body privacy guard", err)
	}
	if calls != 1 {
		t.Fatal("redirect followed with credentials")
	}
	for _, n := range []int{maxBody, maxBody + 1} {
		c.http.Transport = roundTrip(func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(strings.Repeat(" ", n)))}, nil
		})
		_, err = c.JSON(t.Context(), Request{Method: "GET", Path: "/value"}, nil)
		if (err == nil) != (n == maxBody) {
			t.Fatal("response bound", n, err)
		}
	}
	ctx, cancel = context.WithCancel(t.Context())
	cancel()
	if _, err = c.JSON(ctx, Request{Method: "GET", Path: "/value"}, nil); err == nil {
		t.Fatal("canceled request accepted")
	}
}
func TestDefaultTransportHasOnlyExplicitTrust(t *testing.T) {
	pub, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 11, 17, 0, 0, 0, 0, time.UTC)
	cert := &x509.Certificate{SerialNumber: big.NewInt(1), NotBefore: now, NotAfter: now.Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	der, err := x509.CreateCertificate(rand.Reader, cert, cert, pub, key)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("HTTPS_PROXY", "https://proxy.oberth.svc")
	c, err := New(Connection{URL: "https://store.oberth.svc", ServerName: "store.oberth.svc", CA: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})})
	if err != nil {
		t.Fatal(err)
	}
	transport := c.http.Transport.(*http.Transport)
	if transport.Proxy != nil || transport.TLSClientConfig.InsecureSkipVerify || transport.TLSClientConfig.ServerName != "store.oberth.svc" {
		t.Fatal("ambient or unverified transport")
	}
}
func TestRefusalCatalogueBounds(t *testing.T) {
	for _, code := range []string{"sealed", "address", "trust", "connect", "request", "response", "limit", "drift", "token", "delivery", "path", "input"} {
		a, b := refusal(code)
		if len(a) > 80 || len(b) > 120 || strings.HasSuffix(a, "...") {
			t.Fatal(code)
		}
	}
	checkMountedFileRefusalBounds(t)
}

func checkMountedFileRefusalBounds(t *testing.T) {
	t.Helper()
	for _, field := range []string{storeCAField, storeTokenField} {
		for _, state := range []string{"absolute", "missing", "regular", "empty", "large", "unreadable"} {
			reason, remedy := fileRefusal(field, state)
			if len(reason) > 80 || len(remedy) > 120 || strings.HasSuffix(reason, "...") || strings.HasSuffix(remedy, "...") {
				t.Fatal(field, state, "message exceeds its bound")
			}
		}
	}
}

func TestRequestBodyBoundBeforeHTTP(t *testing.T) {
	for _, extra := range []int{0, 1} {
		called := false
		client, err := New(Connection{URL: "https://store.oberth.svc", Transport: roundTrip(func(*http.Request) (*http.Response, error) {
			called = true
			return &http.Response{StatusCode: 204, Body: io.NopCloser(strings.NewReader(""))}, nil
		})})
		if err != nil {
			t.Fatal(err)
		}
		_, err = client.JSON(t.Context(), Request{Method: "POST", Path: "/value", Body: strings.Repeat("a", maxBody-2+extra)}, nil)
		if (err == nil) != (extra == 0) || called != (extra == 0) {
			t.Fatal("request body bound", extra, err)
		}
	}
}
