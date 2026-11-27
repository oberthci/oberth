//go:build linux

// These helpers exercise Linux server composition.

package main

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/client"
	"github.com/oberthci/oberth/internal/clientcert/issuer"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/publicerr"
)

func (f *composedFixture) certificateAccess(t *testing.T, certificate tls.Certificate, allowed bool) {
	t.Helper()
	c, err := client.New(client.Config{Options: client.Options{URL: f.config.Public.URL}, Now: f.deps.clock.Now, Transport: f.client(f.config.Listen.HTTPS, certificate).Transport})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	err = c.Invoke(t.Context(), "run_list", apicontract.RunListArgs{Limit: 1}, nil)
	if allowed {
		if err != nil {
			t.Fatal("joined certificate refused", err)
		}
		return
	}
	p := publicerr.Classify(err)
	if err == nil || p.Reason() != "Caller credentials are invalid." {
		t.Fatal("server credential rejection was misdiagnosed", err)
	}
}
func (f *composedFixture) joinedCertificate(t *testing.T) tls.Certificate {
	t.Helper()
	dir := filepath.Dir(f.identity)
	certificate, err := tls.LoadX509KeyPair(filepath.Join(dir, "admin.crt"), filepath.Join(dir, "admin.key"))
	if err != nil {
		t.Fatal(err)
	}
	return certificate
}
func (f *composedFixture) unknownCertificate(t *testing.T) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	csr, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{}, key)
	if err != nil {
		t.Fatal(err)
	}
	authority, err := issuer.Load(f.ca, f.key, rand.Reader, f.deps.clock.Now)
	if err != nil {
		t.Fatal(err)
	}
	certificate, _, err := authority.Issue("unregistered", pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: csr}), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	secret, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	pair, err := tls.X509KeyPair(certificate, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: secret}))
	if err != nil {
		t.Fatal(err)
	}
	return pair
}
func (f *composedFixture) checkReservationRetry(t *testing.T) {
	t.Helper()
	invitation := f.invitation(t, "retry-agent")
	request := model.InviteRedemption{Secret: invitation.Secret, ServerURL: invitation.ServerURL, CAPin: invitation.CAPin, CSR: "malformed", SSHPublicKey: "malformed"}
	body, _ := json.Marshal(modernMCPFrame(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{"name": "uplink_redeem", "arguments": request}}))
	var wire apicontract.MCPRequest
	_ = json.Unmarshal(body, &wire)
	for attempt := range 2 {
		req, err := http.NewRequestWithContext(t.Context(), "POST", f.config.Public.URL+"/mcp/enroll", bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header = wire.Headers()
		req.Header.Set("Content-Type", "application/json")
		response, err := f.client(f.config.Listen.HTTPS).Do(req)
		if err != nil {
			t.Fatal(err)
		}
		raw, err := io.ReadAll(response.Body)
		_ = response.Body.Close()
		if err != nil {
			t.Fatal(err)
		}
		payload, refused, _, err := apicontract.ToolPayload(raw)
		if err != nil || !refused {
			t.Fatal("malformed enrollment accepted", string(raw), err)
		}
		var problem struct{ Reason, Remedy string }
		if err = json.Unmarshal(payload, &problem); err != nil {
			t.Fatal(err)
		}
		if attempt == 1 && (!strings.Contains(problem.Reason, "temporarily reserved") || !strings.Contains(problem.Remedy, "Wait one minute")) {
			t.Fatal("retry lacks actionable reservation message", problem)
		}
	}
}
