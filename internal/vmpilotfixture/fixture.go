// Package vmpilotfixture owns ephemeral synthetic Beacon capabilities. It has
// no filesystem, Kubernetes, process, network or scheduler integration.
package vmpilotfixture

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"math/big"
	"net/netip"
	"sync"
	"time"

	"github.com/oberthci/oberth/internal/vmrunner"
)

var (
	ErrUnavailable = errors.New("vmpilotfixture: fixture unavailable")
	ErrBinding     = errors.New("vmpilotfixture: invalid or already consumed binding")
	ErrGeneration  = errors.New("vmpilotfixture: capability generation failed")
	ErrDelivery    = errors.New("vmpilotfixture: capability delivery failed")
)

// Tenant contains only a synthetic capability, never a real tenant secret.
// Borrowed byte slices must not be retained past the delivery callback.
type Tenant struct {
	ID  string
	Key []byte
}

func (Tenant) String() string               { return "[synthetic tenant capability]" }
func (Tenant) GoString() string             { return "[synthetic tenant capability]" }
func (Tenant) MarshalJSON() ([]byte, error) { return nil, ErrBinding }

// GuestMaterial is borrowed by a future independently reviewed memory-only
// delivery adapter. It carries neither the CA signing key nor result authority.
// It deliberately has no general-purpose JSON representation.
type GuestMaterial struct {
	Endpoint       vmrunner.PilotEndpoint
	CertificatePEM []byte
	PrivateKeyPEM  []byte
	Tenants        [2]Tenant
}

func (GuestMaterial) String() string               { return "[synthetic guest capabilities]" }
func (GuestMaterial) GoString() string             { return "[synthetic guest capabilities]" }
func (GuestMaterial) MarshalJSON() ([]byte, error) { return nil, ErrBinding }

func (m *GuestMaterial) clear() {
	clear(m.CertificatePEM)
	clear(m.PrivateKeyPEM)
	for i := range m.Tenants {
		clear(m.Tenants[i].Key)
	}
}

type leaf struct {
	endpoint  vmrunner.PilotEndpoint
	cert      []byte
	key       []byte
	delivered bool
}

// Fixture must not be copied. Close waits for delivery callbacks and clears
// all buffers it owns. Go/runtime copies and a caller that copies borrowed bytes
// are outside that guarantee; this is not proof of guest transport custody.
type Fixture struct {
	mu           sync.Mutex
	plan         vmrunner.PilotPlan
	created      time.Time
	expires      time.Time
	generation   string
	ca           *x509.Certificate
	caPEM        []byte
	caKey        ed25519.PrivateKey
	tenants      [2]Tenant
	leaves       [2]leaf
	bootstrapped bool
	closed       bool
}

func (*Fixture) String() string               { return "[ephemeral Beacon fixture]" }
func (*Fixture) GoString() string             { return "[ephemeral Beacon fixture]" }
func (*Fixture) MarshalJSON() ([]byte, error) { return nil, ErrBinding }

// New fixes all capabilities to one immutable admitted plan. Neither a digest
// shaped image reference nor this constructor verifies producer signatures.
func New(plan vmrunner.PilotPlan) (*Fixture, error) {
	if vmrunner.ValidatePilotPlan(plan) != nil || plan.Spec.Deadline < 30*time.Second {
		return nil, ErrBinding
	}
	now := time.Now().UTC()
	f := &Fixture{plan: plan, created: now, expires: now.Add(plan.Spec.Deadline + time.Minute)}
	ok := false
	defer func() {
		if !ok {
			f.Close()
		}
	}()
	var generation [32]byte
	if _, err := rand.Read(generation[:]); err != nil {
		return nil, ErrGeneration
	}
	f.generation = hex.EncodeToString(generation[:])
	for i := range f.tenants {
		var id [16]byte
		if _, err := rand.Read(id[:]); err != nil {
			return nil, ErrGeneration
		}
		f.tenants[i] = Tenant{ID: hex.EncodeToString(id[:]), Key: make([]byte, 32)}
		if _, err := rand.Read(f.tenants[i].Key); err != nil {
			return nil, ErrGeneration
		}
	}
	if f.tenants[0].ID == f.tenants[1].ID || bytes.Equal(f.tenants[0].Key, f.tenants[1].Key) {
		return nil, ErrGeneration
	}
	pub, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, ErrGeneration
	}
	f.caKey = key
	serial, err := newSerial()
	if err != nil {
		return nil, err
	}
	template := &x509.Certificate{SerialNumber: serial, Subject: pkix.Name{CommonName: "ephemeral Beacon fixture"},
		NotBefore: now.Add(-time.Minute), NotAfter: f.expires, IsCA: true,
		BasicConstraintsValid: true, MaxPathLen: 0, MaxPathLenZero: true,
		KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature}
	der, err := x509.CreateCertificate(rand.Reader, template, template, pub, key)
	if err != nil {
		return nil, ErrGeneration
	}
	f.ca, err = x509.ParseCertificate(der)
	if err != nil {
		return nil, ErrGeneration
	}
	f.caPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	ok = true
	return f, nil
}

func newSerial() (*big.Int, error) {
	n, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, ErrGeneration
	}
	return n.Add(n, big.NewInt(1)), nil
}

func slot(attempt string) (int, bool) {
	switch attempt {
	case vmrunner.InitialVMResource:
		return 0, true
	case vmrunner.RestartVMResource:
		return 1, true
	default:
		return 0, false
	}
}

func (f *Fixture) available() bool { return !f.closed && time.Now().Before(f.expires) }

// BindEndpoint fixes the host-observed identities and literal port443 address
// once per VM slot, minting an independent leaf key. The caller must prove the
// observations and old-resource cleanup through the durable runtime journal;
// this function does not confer restart authority or observe Kubernetes.
func (f *Fixture) BindEndpoint(attempt, vmiUID, launcherUID, address string) (vmrunner.PilotEndpoint, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.available() {
		return vmrunner.PilotEndpoint{}, ErrUnavailable
	}
	i, ok := slot(attempt)
	if !ok || f.leaves[i].endpoint != (vmrunner.PilotEndpoint{}) {
		return vmrunner.PilotEndpoint{}, ErrBinding
	}
	ep := vmrunner.PilotEndpoint{Attempt: attempt, VMIUID: vmiUID, LauncherUID: launcherUID, Address: address,
		ServerName: "beacon.fixture", CertificateSHA256: f.generation, FixtureGeneration: f.generation}
	ap, err := netip.ParseAddrPort(address)
	if err != nil || ap.Port() != 443 || vmrunner.ValidatePilotEndpoint(ep) != nil {
		return vmrunner.PilotEndpoint{}, ErrBinding
	}
	old := f.leaves[0].endpoint
	if i == 1 && (old == (vmrunner.PilotEndpoint{}) || old.VMIUID == vmiUID || old.LauncherUID == launcherUID) {
		return vmrunner.PilotEndpoint{}, ErrBinding
	}
	pub, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return vmrunner.PilotEndpoint{}, ErrGeneration
	}
	defer clear(key)
	serial, err := newSerial()
	if err != nil {
		return vmrunner.PilotEndpoint{}, err
	}
	template := &x509.Certificate{SerialNumber: serial, DNSNames: []string{"beacon.fixture"},
		NotBefore: f.created.Add(-time.Minute), NotAfter: f.expires,
		BasicConstraintsValid: true, KeyUsage: x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, template, f.ca, pub, f.caKey)
	if err != nil {
		return vmrunner.PilotEndpoint{}, ErrGeneration
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return vmrunner.PilotEndpoint{}, ErrGeneration
	}
	defer clear(keyDER)
	digest := sha256.Sum256(der)
	ep.CertificateSHA256 = hex.EncodeToString(digest[:])
	f.leaves[i] = leaf{endpoint: ep, cert: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		key: pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})}
	if i == 1 {
		clear(f.caKey) // The closed recipe admits exactly two leaf certificates.
		f.caKey = nil
	}
	return ep, nil
}

// WithGuestMaterial lends each bound VM's material at most once, including a
// failed delivery. The callback must not reenter Fixture methods or copy bytes
// beyond its lifetime. Borrowed copies are cleared before return, even on error.
// Adapter errors are intentionally replaced with a static error, never echoed.
func (f *Fixture) WithGuestMaterial(endpoint vmrunner.PilotEndpoint, deliver func(GuestMaterial) error) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.available() {
		return ErrUnavailable
	}
	i, ok := slot(endpoint.Attempt)
	if !ok || deliver == nil || f.leaves[i].endpoint != endpoint || f.leaves[i].delivered {
		return ErrBinding
	}
	l := &f.leaves[i]
	l.delivered = true
	m := GuestMaterial{Endpoint: endpoint, CertificatePEM: bytes.Clone(l.cert), PrivateKeyPEM: bytes.Clone(l.key)}
	for i, tenant := range f.tenants {
		m.Tenants[i] = Tenant{ID: tenant.ID, Key: bytes.Clone(tenant.Key)}
	}
	defer m.clear()
	defer clear(l.key)
	if err := deliver(m); err != nil {
		return ErrDelivery
	}
	return nil
}

// Close is idempotent. It clears owned secrets, then refuses further bindings
// or delivery; a process crash cannot reconstruct these synthetic capabilities.
func (f *Fixture) Close() {
	f.mu.Lock()
	defer f.mu.Unlock()
	clear(f.caKey)
	for i := range f.tenants {
		clear(f.tenants[i].Key)
	}
	for i := range f.leaves {
		clear(f.leaves[i].key)
	}
	f.closed = true
}
