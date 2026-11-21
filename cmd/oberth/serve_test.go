package main

import (
	"context"
	"encoding/base64"
	"errors"
	"io"

	"net"

	"sync"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/actor"

	"github.com/oberthci/oberth/internal/api/fake"
	"github.com/oberthci/oberth/internal/apicontract"

	"github.com/oberthci/oberth/internal/stateport"
	"github.com/oberthci/oberth/internal/store"
)

type closedListener struct {
	closed  chan struct{}
	once    sync.Once
	onClose func()
}

func (l *closedListener) Accept() (net.Conn, error) { <-l.closed; return nil, net.ErrClosed }
func (l *closedListener) Close() error {
	l.once.Do(func() {
		close(l.closed)
		if l.onClose != nil {
			l.onClose()
		}
	})
	return nil
}
func (l *closedListener) Addr() net.Addr { return &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 8443} }

func testComposition(t *testing.T) *composition {
	t.Helper()
	clock := fake.NewClock()
	cfg, err := decodeConfig(validConfig())
	if err != nil {
		t.Fatal(err)
	}
	c := &composition{config: cfg, clock: serverClock{clock.Now, clock.Ticker}, bindings: fake.Bindings(), runtime: clock.Runtime()}
	c.health = &health{clock: c.clock, sample: func(context.Context) error { return nil }, validLeaf: func() bool { return true }}
	c.auth = fake.NewAuth(apicontract.Caller{Actor: actor.Actor{Identity: "operator", Admin: true, AuthMethod: "bearer"}, CredentialID: "id"})
	if err = newTransport(context.Background(), c, io.Discard); err != nil {
		t.Fatal(err)
	}
	return c
}

func TestOpenLandedStorageAndBackup(t *testing.T) {
	clock := fake.NewClock()
	path := t.TempDir() + "/state.db"
	c := testComposition(t)
	c.config.Protocol = stateport.Protocol{SchemaIdentity: "sample-schema", AuditDomain: "sample-audit"}
	options := store.DefaultOptions(clock.Now)
	options.Protocol = c.config.Protocol
	db, err := store.Create(context.Background(), path, options)
	if err != nil {
		t.Fatal(err)
	}
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}
	c.config.DatabaseFile = path
	parts, err := openStorage(context.Background(), c)
	if err != nil {
		t.Fatal(err)
	}
	life := newLifecycle(t.Context(), parts)
	defer func() {
		if err := life.stop(context.Background()); err != nil {
			t.Error(err)
		}
	}()
	if c.logs == nil || c.backup == nil || c.store.Readiness(context.Background()) != nil {
		t.Fatal("landed storage not composed")
	}
	if _, err = c.store.Snapshot(context.Background(), stateport.Options{Protocol: c.config.Protocol, Schema: store.Schema}, io.Discard); err != nil {
		t.Fatal(err)
	}
	if err = c.logs.Append(t.Context(), "probe", []byte("data")); err != nil {
		t.Fatal(err)
	}
	if err = life.stop(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err = c.logs.Append(t.Context(), "probe", []byte("data")); err == nil {
		t.Fatal("logs left open")
	}
}

func TestStoreAuthUsesCanonicalSPKI(t *testing.T) {
	clock := fake.NewClock()
	db, err := store.Create(context.Background(), t.TempDir()+"/state.db", store.DefaultOptions(clock.Now))
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := db.Close(); err != nil {
			t.Error(err)
		}
	}()
	cert, key := authority(t, clock.Now())
	identity, err := newServerIdentity(cert, key, []string{"oberth.local"}, clock.Now)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := identity.certificate(nil)
	if err != nil {
		t.Fatal(err)
	}
	action := stateport.Action{Actor: "installer"}
	if err = db.RegisterKey(context.Background(), action, stateport.Key{Identity: "operator", Fingerprint: "fingerprint", Kind: "operator", Admin: true}); err != nil {
		t.Fatal(err)
	}
	if err = db.AddCertificatePin(context.Background(), action, stateport.CertificateBinding{Fingerprint: "fingerprint", SPKI: base64.StdEncoding.EncodeToString(leaf.Leaf.RawSubjectPublicKeyInfo), ExpiresAt: leaf.Leaf.NotAfter}); err != nil {
		t.Fatal(err)
	}
	caller, err := (storeAuth{db}).Certificate(context.Background(), leaf.Leaf)
	if err != nil || caller.Actor.Identity != "operator" {
		t.Fatal("canonical pin not found", err)
	}
}

func TestFailedSecondBindJoinsFirstListener(t *testing.T) {
	c := testComposition(t)
	l := newLifecycle(context.Background(), nil)
	clock := fake.NewClock()
	cert, key := authority(t, clock.Now())
	identity, err := newServerIdentity(cert, key, []string{"oberth.local"}, clock.Now)
	if err != nil {
		t.Fatal(err)
	}
	first := &closedListener{closed: make(chan struct{})}
	binds := 0
	refusal := errors.New("second bind refused")
	listen := func(context.Context, string, string) (net.Listener, error) {
		binds++
		if binds == 2 {
			return nil, refusal
		}
		return first, nil
	}
	deps := serverDependencies{listen: listen, timeout: context.WithTimeout}
	s := newServing(c, l, deps)
	err = s.listen(context.Background(), identity)
	_ = s.close(context.Background())
	if !errors.Is(err, refusal) {
		t.Fatal("bind error lost")
	}
	select {
	case <-first.closed:
	default:
		t.Fatal("first listener left open")
	}
}

func TestStoreAuthUsesCanonicalBearerDigest(t *testing.T) {
	clock := fake.NewClock()
	db, err := store.Create(t.Context(), t.TempDir()+"/state.db", store.DefaultOptions(clock.Now))
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := db.Close(); err != nil {
			t.Error(err)
		}
	}()
	action := stateport.Action{Actor: "installer"}
	if err = db.RegisterKey(t.Context(), action, stateport.Key{Identity: "operator", Fingerprint: "fingerprint", Kind: "operator", Admin: true}); err != nil {
		t.Fatal(err)
	}
	digest := apicontract.SecretDigest("abc")
	if err = db.AddCredential(t.Context(), action, stateport.Credential{Digest: digest, Fingerprint: "fingerprint", ExpiresAt: clock.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	caller, err := (storeAuth{db}).Bearer(t.Context(), "abc")
	if err != nil || caller.Actor.Identity != "operator" || caller.CredentialID != digest {
		t.Fatal("wire bearer digest not used", err)
	}
}
