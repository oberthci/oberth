package client

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/api/fake"
	"github.com/oberthci/oberth/internal/publicerr"
)

func TestExpiredClientCertificateRefusesBeforeDial(t *testing.T) {
	cert, key := certificate(t, 1)
	leaf, err := x509.ParseCertificate(mustPEM(t, cert))
	if err != nil {
		t.Fatal(err)
	}
	path := privateFile(t, "client.crt", cert)
	c, err := New(Config{Options: Options{URL: "https://oberth.local", ClientCert: path, ClientKey: privateFile(t, "client.key", key)}, Now: func() time.Time { return leaf.NotAfter }})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	dials := 0
	c.http.Transport.(*http.Transport).DialContext = func(context.Context, string, string) (net.Conn, error) { dials++; return nil, context.Canceled }
	_, _, err = c.Call(t.Context(), definition(t, "repo_list"), json.RawMessage(`{}`))
	p := publicerr.Classify(err)
	if dials != 0 || p.Reason() != "Client certificate has expired." || !strings.Contains(p.Remedy(), "oberth renew") || !strings.HasPrefix(p.Remedy(), "Ask an administrator for a new invitation and join again;") || !strings.Contains(p.Remedy(), "only while the certificate is still valid") || len(p.Context) != 1 || !strings.Contains(p.Context[0], leaf.NotAfter.UTC().Format(time.RFC3339)) {
		t.Fatal("expiry was not diagnosed locally", dials, p, p.Context)
	}
}
func mustPEM(t *testing.T, data []byte) []byte {
	t.Helper()
	block, _ := pem.Decode(data)
	if block == nil {
		t.Fatal("missing certificate")
	}
	return block.Bytes
}

func TestServerClientCertificateRejectionIsNotAConnectionFailure(t *testing.T) {
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("rejected certificate reached handler") }))
	server.TLS = &tls.Config{ClientAuth: tls.RequireAndVerifyClientCert, ClientCAs: x509.NewCertPool()}
	server.StartTLS()
	defer server.Close()
	cert, key := certificate(t, 1)
	ca := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})
	c, err := New(Config{Options: Options{URL: server.URL, CAFile: privateFile(t, "server-ca.crt", ca), ClientCert: privateFile(t, "client.crt", cert), ClientKey: privateFile(t, "client.key", key)}, Now: fake.NewClock().Now})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	_, _, err = c.Call(ctx, definition(t, "repo_list"), json.RawMessage(`{}`))
	p := publicerr.Classify(err)
	if p.Reason() != "Server refused this client certificate." || !strings.Contains(p.Remedy(), "new invitation") {
		t.Fatal("remote TLS rejection misclassified", p)
	}
}
