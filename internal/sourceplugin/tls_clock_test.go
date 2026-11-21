package sourceplugin

import (
	"bufio"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"io"
	"math/big"
	"net"
	"net/http"
	"testing"
	"time"
)

func TestSourceTLSUsesTheConfiguredCertificateClock(t *testing.T) {
	cfg := config(t)
	key := ed25519.NewKeyFromSeed(make([]byte, ed25519.SeedSize))
	cert := &x509.Certificate{SerialNumber: big.NewInt(2), DNSNames: []string{"source"}, NotBefore: time.Unix(50, 0), NotAfter: time.Unix(150, 0), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, cert, cert, key.Public(), key)
	if err != nil {
		t.Fatal(err)
	}
	cfg.CAPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	done := make(chan struct{})
	cfg.DialContext = func(ctx context.Context, _, _ string) (net.Conn, error) {
		client, server := net.Pipe()
		go func() {
			defer close(done)
			defer server.Close()
			stop := context.AfterFunc(ctx, func() { _ = server.Close() })
			defer stop()
			conn := tls.Server(server, &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}})
			if request, err := http.ReadRequest(bufio.NewReader(conn)); err == nil {
				_ = request.Body.Close()
				_, _ = io.WriteString(conn, "HTTP/1.1 200 OK\r\nContent-Length: 2\r\n\r\nok")
			}
		}()
		return client, nil
	}
	c, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	request, err := http.NewRequestWithContext(t.Context(), "GET", cfg.Endpoint, nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := c.http.Do(request)
	if err != nil {
		t.Fatal("certificate valid at the injected time was refused", err)
	}
	_ = response.Body.Close()
	<-done
}
