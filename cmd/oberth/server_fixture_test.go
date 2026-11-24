package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/actor"
	"github.com/oberthci/oberth/internal/admissioncontract"
	"github.com/oberthci/oberth/internal/api/fake"
	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/cronsync"
	cronfake "github.com/oberthci/oberth/internal/cronsync/fake"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/store"
)

type memoryListener struct {
	incoming chan net.Conn
	closed   chan struct{}
	once     sync.Once
	onClose  func()
}

func (l *memoryListener) Accept() (net.Conn, error) {
	select {
	case c := <-l.incoming:
		return c, nil
	case <-l.closed:
		return nil, net.ErrClosed
	}
}
func (l *memoryListener) Close() error {
	l.once.Do(func() {
		close(l.closed)
		if l.onClose != nil {
			l.onClose()
		}
	})
	return nil
}
func (*memoryListener) Addr() net.Addr { return &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 8443} }
func (l *memoryListener) dial(ctx context.Context, _, _ string) (net.Conn, error) {
	client, server := net.Pipe()
	select {
	case l.incoming <- testServerConn{server}:
		return client, nil
	case <-ctx.Done():
		_ = client.Close()
		_ = server.Close()
		return nil, ctx.Err()
	case <-l.closed:
		_ = client.Close()
		_ = server.Close()
		return nil, net.ErrClosed
	}
}

// The API's injected clock also dates writes. Memory pipes have no wall-clock
// write deadline; request cancellation and listener closure bound their work.
type testServerConn struct{ net.Conn }

func (testServerConn) SetWriteDeadline(time.Time) error { return nil }

type serverTrace struct {
	mu     sync.Mutex
	events []string
	text   strings.Builder
	ready  chan struct{}
	once   sync.Once
}

func (s *serverTrace) record(event string) {
	s.mu.Lock()
	s.events = append(s.events, event)
	s.mu.Unlock()
}
func (s *serverTrace) snapshot() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.events...)
}
func (s *serverTrace) String() string { s.mu.Lock(); defer s.mu.Unlock(); return s.text.String() }
func (s *serverTrace) Write(data []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.text.Write(data)
	if strings.HasPrefix(string(data), "ready:") {
		s.once.Do(func() { close(s.ready) })
	}
	return len(data), nil
}

type serverFixture struct {
	t                      *testing.T
	clock                  *fake.Clock
	config                 serverConfig
	deps                   serverDependencies
	trace                  *serverTrace
	c                      *composition
	ca, key                []byte
	listeners              map[string]*memoryListener
	contexts               map[string]context.Context
	mu                     sync.Mutex
	source                 http.Handler
	beforeCache            func(*composition) error
	readyError, closeError error
	sshRun                 func(context.Context, net.Listener) error
	sshContext             chan context.Context
}

func fixtureServer(t *testing.T) *serverFixture {
	t.Helper()
	clock := fake.NewClock()
	cfg, err := decodeConfig(validConfig())
	if err != nil {
		t.Fatal(err)
	}
	cfg.DatabaseFile = filepath.Join(t.TempDir(), "state.db")
	cfg.Protocol.SchemaIdentity = "slice2-schema"
	cfg.Protocol.AuditDomain = "slice2-audit"
	options := store.DefaultOptions(clock.Now)
	options.Protocol = cfg.Protocol
	db, err := store.Create(t.Context(), cfg.DatabaseFile, options)
	if err != nil {
		t.Fatal(err)
	}
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}
	f := &serverFixture{t: t, clock: clock, config: cfg, trace: &serverTrace{ready: make(chan struct{})}, listeners: map[string]*memoryListener{}, contexts: map[string]context.Context{}, sshContext: make(chan context.Context, 1), source: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(204) })}
	f.ca, f.key = authority(t, clock.Now())
	f.deps = serverDependencies{clock: serverClock{clock.Now, clock.Ticker}, listen: f.listen, timeout: context.WithTimeout}
	caDir, adminDir := t.TempDir(), t.TempDir()
	f.config.Files.CACertificate, f.config.Files.CAPrivateKey = filepath.Join(caDir, "tls.crt"), filepath.Join(caDir, "tls.key")
	f.config.Files.SecretStoreCA, f.config.Files.SecretStoreToken = filepath.Join(caDir, "store.crt"), filepath.Join(caDir, "store.token")
	f.config.Files.AdminPublicKey, f.config.Files.AdminPushKey = filepath.Join(adminDir, "admin.pub"), filepath.Join(adminDir, "push.pub")
	for path, data := range map[string][]byte{f.config.Files.CACertificate: f.ca, f.config.Files.CAPrivateKey: f.key, f.config.Files.SecretStoreCA: f.ca, f.config.Files.SecretStoreToken: []byte("server-projected-token")} {
		if err = os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	writeAdminKey(t, adminDir, "admin.pub")
	writeAdminKey(t, adminDir, "push.pub")
	f.config.Files.SSHHostKey, _ = composeSSHKey(t, t.TempDir(), "host-key")
	f.config.Files.ForgeKey, _ = composeSSHKey(t, t.TempDir(), "forge-key")
	f.config.Runtime = validRuntime()
	f.healthyStore(t)

	f.deps.constructors = constructors{CacheNew: f.cache, NativeNew: f.native, WiringNew: f.wiring, ServiceNew: f.service, SourceNew: f.delivery}
	return f
}
func (f *serverFixture) listen(_ context.Context, _, address string) (net.Listener, error) {
	f.trace.record("bind " + address)
	l := &memoryListener{incoming: make(chan net.Conn), closed: make(chan struct{}), onClose: func() { f.trace.record("drain " + address) }}
	f.mu.Lock()
	f.listeners[address] = l
	f.mu.Unlock()
	return l, nil
}
func (f *serverFixture) listener(address string) *memoryListener {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.listeners[address]
}
func (f *serverFixture) part(ctx context.Context, name string) Part {
	f.trace.record("construct " + name)
	f.mu.Lock()
	f.contexts[name] = ctx
	f.mu.Unlock()
	entered := make(chan struct{})
	return Part{Name: name, Run: func(run context.Context) error {
		if run != ctx {
			return fmt.Errorf("constructor lifetime differs")
		}
		f.trace.record("run " + name)
		close(entered)
		<-ctx.Done()
		f.trace.record("end " + name)
		return ctx.Err()
	}, Ready: func(phase context.Context) error {
		select {
		case <-entered:
		case <-phase.Done():
			return phase.Err()
		}
		f.trace.record("ready " + name)
		if name == "native" {
			return f.readyError
		}
		return nil
	}, Recover: func(context.Context) error { f.trace.record("recover " + name); return nil }, Close: func(context.Context) error {
		f.trace.record("close " + name)
		if name == "service" {
			return f.closeError
		}
		return nil
	}}
}
func (f *serverFixture) cache(ctx context.Context, c *composition) (Part, error) {
	f.c = c
	if f.beforeCache != nil {
		if err := f.beforeCache(c); err != nil {
			return Part{}, err
		}
	}
	c.cacheGC = func(context.Context) error { f.trace.record("cache GC"); return nil }
	c.replayPending = func(context.Context) error { f.trace.record("replay pending pushes"); return nil }
	p := f.part(ctx, "cache")
	p.Ready = nil
	p.Recover = nil
	return p, nil
}
func (f *serverFixture) native(ctx context.Context, c *composition) (Part, error) {
	c.watch = func(ctx context.Context, ready func()) error {
		f.trace.record("watch ready")
		ready()
		<-ctx.Done()
		f.trace.record("end watch")
		return ctx.Err()
	}
	return f.part(ctx, "native"), nil
}
func (f *serverFixture) wiring(ctx context.Context, c *composition) (Part, error) {
	c.scheduleConfig = cronsync.Config{Client: cronfake.NewNative(c.config.PipelineNamespace, c.clock.Now, nil), Git: cronfake.NewGit(nil), Repositories: func(context.Context) ([]cronsync.Repository, error) { return nil, nil }, Repository: func(context.Context, string) (cronsync.Repository, bool, error) {
		return cronsync.Repository{}, false, nil
	}, Admit: func(context.Context, cronsync.Preparation) (*admissioncontract.PreparedAdmission, error) {
		return nil, nil
	}, PauseIntent: func(context.Context, string, string) (bool, error) { return false, nil }}
	return f.part(ctx, "wiring"), nil
}
func (f *serverFixture) service(ctx context.Context, c *composition) (Part, error) {
	c.bindings = fake.Bindings()
	c.views = apicontract.Views{Counts: apicontract.Binding[apicontract.Empty, apicontract.RunCounts]{Handler: &fake.Handler[apicontract.Empty, apicontract.RunCounts]{}}, Containers: apicontract.Binding[apicontract.IDArgs, apicontract.ContainerPage]{Handler: &fake.Handler[apicontract.IDArgs, apicontract.ContainerPage]{}}, ContainerLog: apicontract.Binding[apicontract.ContainerArgs, apicontract.LogResult]{Handler: &fake.Handler[apicontract.ContainerArgs, apicontract.LogResult]{}}}
	c.auth = fake.NewAuth(apicontract.Caller{Actor: actor.Actor{Identity: "operator", Admin: true, AuthMethod: "bearer"}, CredentialID: "id"})
	c.installAdmin = func(context.Context, []byte, []byte) error { f.trace.record("install administrator"); return nil }
	c.enroll = &fake.Handler[model.InviteRedemption, model.InviteCertificate]{}
	c.streams = &fake.Streams{}
	p := f.part(ctx, "service")
	p.Shutdown = stopService
	return p, nil
}
func (f *serverFixture) delivery(ctx context.Context, c *composition) (Part, error) {
	c.source = f.source
	c.ssh = func(ctx context.Context, l net.Listener) error {
		f.trace.record("ssh run")
		f.sshContext <- ctx
		stop := context.AfterFunc(ctx, func() { _ = l.Close() })
		defer stop()
		if f.sshRun != nil {
			return f.sshRun(ctx, l)
		}
		_, err := l.Accept()
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return err
	}
	p := f.part(ctx, "source")
	p.Run = nil
	p.Ready = nil
	return p, nil
}
func (f *serverFixture) client(address string, certificates ...tls.Certificate) *http.Client {
	f.t.Helper()
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(f.ca) {
		f.t.Fatal("invalid fixture authority")
	}
	l := f.listener(address)
	if l == nil {
		f.t.Fatal("listener not bound")
	}
	transport := &http.Transport{DialContext: l.dial, DisableKeepAlives: true, TLSClientConfig: &tls.Config{RootCAs: roots, ServerName: f.publicHostname(), MinVersion: tls.VersionTLS12, Time: f.clock.Now, Certificates: certificates}}
	f.t.Cleanup(transport.CloseIdleConnections)
	return &http.Client{Transport: transport, Timeout: 10 * time.Second}
}
func (f *serverFixture) publicHostname() string {
	f.t.Helper()
	address, err := url.Parse(f.config.Public.URL)
	if err != nil {
		f.t.Fatal(err)
	}
	return address.Hostname()
}

func (f *serverFixture) start(ctx context.Context) <-chan error {
	f.t.Helper()
	ctx, cancel := context.WithCancel(ctx)
	finished := make(chan struct{})
	f.t.Cleanup(func() {
		cancel()
		select {
		case <-finished:
		case <-time.After(10 * time.Second):
			f.t.Error("server did not stop within fixture budget")
		}
	})
	done := make(chan error, 1)
	go func() { defer close(finished); done <- serve(ctx, f.config, f.deps, f.trace) }()
	select {
	case <-f.trace.ready:
		if err := f.c.health.ready(ctx); err != nil {
			f.t.Fatalf("server announced ready before startup completed: %v", err)
		}
		return done
	case err := <-done:
		f.t.Fatalf("serve failed before ready: %v", err)
	case <-time.After(10 * time.Second):
		f.t.Fatal("server did not become ready within fixture budget")
	}
	return done
}
func readStatus(t *testing.T, client *http.Client, path string) int {
	t.Helper()
	request, err := http.NewRequestWithContext(t.Context(), "GET", "https://oberth.local"+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, response.Body)
	_ = response.Body.Close()
	return response.StatusCode
}
