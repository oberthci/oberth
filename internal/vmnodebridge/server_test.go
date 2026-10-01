package vmnodebridge

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"io"
	"math/big"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/vmrunner"
)

func testCertificates(t *testing.T) ([]byte, tls.Certificate, tls.Certificate) {
	t.Helper()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	caTemplate := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "bridge test CA"},
		NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour), IsCA: true, BasicConstraintsValid: true,
		KeyUsage: x509.KeyUsageCertSign}
	caDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	ca, err := x509.ParseCertificate(caDER)
	if err != nil {
		t.Fatal(err)
	}
	makeLeaf := func(serial int64, dns string, uri string, usage x509.ExtKeyUsage) tls.Certificate {
		t.Helper()
		key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		leaf := &x509.Certificate{SerialNumber: big.NewInt(serial), NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour),
			ExtKeyUsage: []x509.ExtKeyUsage{usage}, KeyUsage: x509.KeyUsageDigitalSignature}
		if dns != "" {
			leaf.DNSNames = []string{dns}
		}
		if uri != "" {
			parsed, err := url.Parse(uri)
			if err != nil {
				t.Fatal(err)
			}
			leaf.URIs = []*url.URL{parsed}
		}
		der, err := x509.CreateCertificate(rand.Reader, leaf, ca, &key.PublicKey, caKey)
		if err != nil {
			t.Fatal(err)
		}
		return tls.Certificate{Certificate: [][]byte{der, caDER}, PrivateKey: key}
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}),
		makeLeaf(2, "worker-1", "", x509.ExtKeyUsageServerAuth),
		makeLeaf(3, "", "spiffe://oberth.ci/operator/vm-attach", x509.ExtKeyUsageClientAuth)
}

type proofStreamer struct{}

func (proofStreamer) Stream(_ context.Context, _ string, input io.Reader, output io.Writer, _ io.Writer) error {
	line, err := bufio.NewReader(input).ReadSlice('\n')
	if err != nil {
		return err
	}
	var frame struct {
		Version     int    `json:"version"`
		Type        string `json:"type"`
		RunID       string `json:"run_id"`
		Execution   string `json:"execution"`
		Attempt     string `json:"attempt"`
		ContainerID string `json:"container_id"`
		Nonce       string `json:"nonce"`
	}
	if err := json.Unmarshal(line[:len(line)-1], &frame); err != nil || frame.Type != "attach-challenge" {
		return errRuntimeStream
	}
	frame.Type = "attach-proof"
	if err := json.NewEncoder(output).Encode(frame); err != nil {
		return err
	}
	return nil
}

func TestProtectedNodeChannelBindsProofAndRejectsReplay(t *testing.T) {
	request, fake, runtime := runtimeFixture(t)
	caPEM, serverCert, clientCert := testCertificates(t)
	serverTLS, err := ServerTLSConfig(serverCert, caPEM, "worker-1")
	if err != nil {
		t.Fatal(err)
	}
	server := &Server{Runtime: runtime, Streamer: proofStreamer{}, Activated: true,
		AllowedClientURI: "spiffe://oberth.ci/operator/vm-attach",
		StreamTLS:        StreamTLS{Origin: "https://127.0.0.1:10010", ServerName: "runtime.test", RootCAs: caPEM}}
	listener := httptest.NewUnstartedServer(server)
	listener.EnableHTTP2 = true
	listener.TLS = serverTLS
	listener.StartTLS()
	defer listener.Close()
	clientTLS, err := ClientTLSConfig(clientCert, caPEM, "worker-1")
	if err != nil {
		t.Fatal(err)
	}
	client, err := NewClient(listener.URL, "worker-1", clientTLS)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	stream, err := client.Open(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	reader, err := vmrunner.ProveConductorStream(ctx, stream.Output, stream.Input, request.Plan, request.Attempt, request.Claim)
	if err != nil {
		t.Fatal(err)
	}
	if !stream.RuntimeStartedAt.Equal(request.Attempt.StartedAt) || fake.attach != 1 {
		t.Fatal("runtime start or exact CRI attach was lost")
	}
	if err := stream.Input.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadAll(reader); err != nil {
		t.Fatalf("authenticated channel closure failed: %v", err)
	}
	_ = reader.Close()
	if duplicate, err := client.Open(ctx, request); err == nil {
		_ = duplicate.Close()
		t.Fatal("one-use node request replayed")
	}
}

func TestStreamURLRequiresPinnedHTTPSOrigin(t *testing.T) {
	caPEM, _, _ := testCertificates(t)
	config := StreamTLS{Origin: "https://127.0.0.1:10010", ServerName: "runtime.test", RootCAs: caPEM}
	if err := config.validateURL("https://127.0.0.1:10010/attach/one-use"); err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{"http://127.0.0.1:10010/attach/one-use", "https://evil.test/attach/one-use",
		"https://127.0.0.1:10010/exec/token", "https://127.0.0.1:10010/attach/one-use/extra",
		"https://127.0.0.1:10010/attach/one-use#fragment", "https://127.0.0.1:10010/attach/one-use?token=x"} {
		if err := config.validateURL(raw); err == nil {
			t.Fatalf("unsafe CRI URL accepted: %s", raw)
		}
	}
}

func TestNodeEndpointRequiresDedicatedClientIdentity(t *testing.T) {
	_, _, clientCert := testCertificates(t)
	leaf, err := x509.ParseCertificate(clientCert.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	server := &Server{Runtime: &Runtime{NodeName: "worker-1"}, Streamer: proofStreamer{}, Activated: true,
		AllowedClientURI: "spiffe://oberth.ci/operator/vm-attach"}
	request := httptest.NewRequest("POST", AttachPath, nil)
	request.ProtoMajor = 2
	request.TLS = &tls.ConnectionState{Version: tls.VersionTLS13, PeerCertificates: []*x509.Certificate{leaf}, VerifiedChains: [][]*x509.Certificate{{leaf}}}
	if !server.authorized(request) {
		t.Fatal("dedicated verified client identity was rejected")
	}
	other := *leaf
	wrongURI, err := url.Parse("spiffe://oberth.ci/operator/other")
	if err != nil {
		t.Fatal(err)
	}
	other.URIs = []*url.URL{wrongURI}
	request.TLS.PeerCertificates = []*x509.Certificate{&other}
	if server.authorized(request) {
		t.Fatal("different client identity was authorized")
	}
	request.TLS.PeerCertificates = []*x509.Certificate{leaf}
	request.ProtoMajor = 1
	if server.authorized(request) {
		t.Fatal("non-HTTP/2 channel was authorized")
	}
}
