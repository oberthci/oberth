package client

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"io"
	"log"
	"net"
	"net/http"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/api/fake"
	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/model"
)

type tlsMemoryListener struct {
	incoming chan net.Conn
	closed   chan struct{}
	once     sync.Once
}

func (l *tlsMemoryListener) Accept() (net.Conn, error) {
	select {
	case c := <-l.incoming:
		return c, nil
	case <-l.closed:
		return nil, net.ErrClosed
	}
}
func (l *tlsMemoryListener) Close() error { l.once.Do(func() { close(l.closed) }); return nil }
func (*tlsMemoryListener) Addr() net.Addr {
	return &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 8443}
}
func (l *tlsMemoryListener) dial(ctx context.Context, _, _ string) (net.Conn, error) {
	client, server := net.Pipe()
	select {
	case l.incoming <- tlsMemoryConn{server}:
		return client, nil
	case <-ctx.Done():
		_ = client.Close()
		_ = server.Close()
		return nil, ctx.Err()
	}
}

type tlsMemoryConn struct{ net.Conn }

func (tlsMemoryConn) SetWriteDeadline(time.Time) error { return nil }

func TestBridgeRenewAndStdout(t *testing.T) {
	cfg, peers, pair, _, rt := tlsBridgeConfig(t)
	dir := filepath.Join(cfg.Base, cfg.Profile)
	p := newBridgePeer(t, cfg)
	p.discover()
	if <-peers != 100 {
		t.Fatal("initial client identity missing")
	}
	p.send(1, "ping", nil)
	p.frame()
	select {
	case <-peers:
		t.Fatal("unchanged identity discarded connection")
	default:
	}
	checkPinnedBridgeTools(t, p)
	renewBridgeIdentity(t, cfg, pair)
	p.send(2, "ping", nil)
	p.frame()
	select {
	case serial := <-peers:
		if serial != 2 {
			t.Fatal("stale client certificate")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("renewal never reached a new TLS handshake")
	}
	if rt.TLSClientConfig.InsecureSkipVerify || rt.Proxy != nil {
		t.Fatal("bridge widened trust")
	}
	other, _ := certificate(t, 101)
	if err := WritePrivate(filepath.Join(dir, "admin.crt"), other); err != nil {
		t.Fatal(err)
	}
	p.send(3, "ping", nil)
	if data := p.frame(); !bytesContainRPCError(data) {
		t.Fatal("mismatched replacement identity accepted")
	}
}

func checkPinnedBridgeTools(t *testing.T, p *bridgePeer) {
	t.Helper()
	for _, name := range []string{"run_get", "run_rerun"} {
		p.tool(name, name, map[string]string{"id": "11111111"}, nil)
		id, _ := json.Marshal(name)
		if _, refused, _, err := apicontract.ToolPayloadForID(p.frame(), id); err != nil || refused {
			t.Fatal("pinned joined tool failed", name, err)
		}
	}
}

func bytesContainRPCError(data []byte) bool {
	var v struct{ Error any }
	return json.Unmarshal(data, &v) == nil && v.Error != nil
}

func tlsBridgeConfig(t *testing.T) (BridgeConfig, chan int64, tls.Certificate, []byte, *http.Transport) {
	t.Helper()
	clock := fake.NewClock()
	ca, key := certificate(t, 100, "oberth.local")
	pair, err := tls.X509KeyPair(ca, key)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM(ca)
	peers := make(chan int64, 8)
	l := &tlsMemoryListener{incoming: make(chan net.Conn), closed: make(chan struct{})}
	serverConfig := &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{pair}, ClientAuth: tls.RequireAndVerifyClientCert, ClientCAs: roots, Time: clock.Now, VerifyConnection: func(s tls.ConnectionState) error { peers <- s.PeerCertificates[0].SerialNumber.Int64(); return nil }}
	s := &http.Server{Handler: server(t, fake.Bindings()), ErrorLog: log.New(io.Discard, "", 0)}
	served := make(chan error, 1)
	go func() { served <- s.Serve(tls.NewListener(l, serverConfig)) }()
	t.Cleanup(func() { _ = s.Close(); <-served })
	cfg := bridgeProfile(t, nil)
	dir := filepath.Join(cfg.Base, cfg.Profile)
	for name, data := range map[string][]byte{"installation.json": []byte(`{"server_url":"https://oberth.local","uplink":true}`), "ca.crt": ca, "admin.crt": ca, "admin.key": key} {
		if err := WritePrivate(filepath.Join(dir, name), data); err != nil {
			t.Fatal(err)
		}
	}
	rt, err := transport(Options{CAFile: filepath.Join(dir, "ca.crt"), ClientCert: filepath.Join(dir, "admin.crt"), ClientKey: filepath.Join(dir, "admin.key")})
	if err != nil {
		t.Fatal(err)
	}
	if !rt.TLSClientConfig.RootCAs.Equal(roots) {
		t.Fatal("bridge trusts authorities outside its pinned profile")
	}
	rt.DialContext = l.dial
	rt.TLSClientConfig.Time = clock.Now
	cfg.Config.Transport = rt
	return cfg, peers, pair, key, rt
}

func renewBridgeIdentity(t *testing.T, cfg BridgeConfig, pair tls.Certificate) {
	t.Helper()
	dir := filepath.Join(cfg.Base, cfg.Profile)
	key, err := ReadPrivate(filepath.Join(dir, "admin.key"))
	if err != nil {
		t.Fatal(err)
	}
	ca, err := ReadPublic(filepath.Join(dir, "ca.crt"))
	if err != nil {
		t.Fatal(err)
	}
	csr, err := RenewalCSR(key)
	if err != nil {
		t.Fatal(err)
	}
	renewed, _ := issueEnrollment(t, model.InviteRedemption{CSR: string(csr)}, enrollmentSigner{pair: pair})
	if err := InstallRenewed(filepath.Join(dir, "admin.crt"), Renewal{Key: key, CA: ca, Certificate: renewed.Certificate, Now: cfg.Config.Now()}); err != nil {
		t.Fatal(err)
	}
}
