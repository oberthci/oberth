package sourceplugin

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"google.golang.org/grpc/codes"
	"math/big"
	"net/http"
	"strings"
	"testing"
	"time"
)

func config(t *testing.T) Config {
	t.Helper()
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	cert := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "source"}, IsCA: true, BasicConstraintsValid: true, NotBefore: time.Unix(0, 0), NotAfter: time.Date(2100, 1, 1, 0, 0, 0, 0, time.UTC), KeyUsage: x509.KeyUsageCertSign}
	der, err := x509.CreateCertificate(rand.Reader, cert, cert, public, private)
	if err != nil {
		t.Fatal(err)
	}
	return Config{Endpoint: "https://source.example", ServerName: "source", CAPEM: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), TokenFile: identity(t), Now: func() time.Time { return time.Unix(100, 0) }}
}
func TestExplicitOriginAndCAHaveNoAmbientTransport(t *testing.T) {
	cfg := config(t)
	client, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	transport := client.http.Transport.(*http.Transport)
	if transport.Proxy != nil || !transport.DisableCompression || transport.TLSClientConfig.RootCAs == nil || transport.TLSClientConfig.ServerName != "source" {
		t.Fatal("ambient transport configured")
	}
	if err := client.http.CheckRedirect(nil, nil); !errors.Is(err, http.ErrUseLastResponse) {
		t.Fatal(err)
	}
	for _, endpoint := range []string{"http://source.example", "https://user:secret@source.example", "https://source.example/path", "https://source.example?selector=other", "https://source.example#fragment", "https://source.example?"} {
		cfg.Endpoint = endpoint
		if c, err := New(cfg); err == nil {
			c.Close()
			t.Fatalf("accepted %s", endpoint)
		}
	}
	cfg = config(t)
	cfg.CAPEM = nil
	if c, err := New(cfg); err == nil {
		c.Close()
		t.Fatal("system roots accepted")
	}
}
func TestNativeArtifactRPCRefusalsAndProcessArguments(t *testing.T) {
	server := &rpcServer{}
	if _, err := server.Save(t.Context(), nil); err == nil {
		t.Fatal("source output accepted")
	}
	if _, err := server.Delete(t.Context(), nil); err == nil {
		t.Fatal("source deletion accepted")
	}
	if _, err := server.IsDirectory(t.Context(), nil); err == nil {
		t.Fatal("missing artifact accepted")
	}
	var out bytes.Buffer
	if code := Run(t.Context(), nil, Config{}, &out); code != 1 || !strings.Contains(out.String(), reasonSocket) {
		t.Fatalf("%d %s", code, out.String())
	}
	if err := Serve(t.Context(), "relative", nil); err == nil {
		t.Fatal("relative socket accepted")
	}
	t.Setenv("OBERTH_SOURCE_ENDPOINT", "https://source.example")

	if cfg := Environment(); cfg.Endpoint != "https://source.example" || cfg.TokenFile != podTokenFile {
		t.Fatal(cfg)
	}
}

func TestPermanentLoadRefusalsKeepRPCStatus(t *testing.T) {
	for _, tc := range []struct {
		reason string
		code   codes.Code
	}{{reasonDenied, codes.PermissionDenied}, {reasonInput, codes.InvalidArgument}, {reasonDisabled, codes.FailedPrecondition}, {reasonReceipt, codes.DataLoss}} {
		if got := loadCode(t.Context(), failure(tc.reason)); got != tc.code {
			t.Fatalf("%s: %v", tc.reason, got)
		}
	}
}
