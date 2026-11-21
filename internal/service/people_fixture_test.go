package service_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"github.com/oberthci/oberth/internal/clientcert/issuer/issuertest"
	"io"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/clientcert"
	"github.com/oberthci/oberth/internal/clientcert/issuer"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/service"
	"github.com/oberthci/oberth/internal/stateport"
	"github.com/oberthci/oberth/internal/store"
	"golang.org/x/crypto/ssh"
)

type peopleFixture struct {
	path    string
	f       *fixture
	state   *store.Store
	admin   *adminStoreAdapter
	publish *testAuthorityPublication
	cluster *testClusterPolicy
	issuer  *testIssuer
}

func people(t *testing.T) *peopleFixture {
	t.Helper()
	f := setup(t)
	state, path := landedStoreWithPath(t, f)
	landedRepository(t, state)
	raw, e := issuertest.GenerateInstallationAuthority(rand.Reader, f.c.Now)
	if e != nil {
		t.Fatal(e)
	}
	authority, e := issuer.Load(raw["ca.crt"], raw["ca.key"], rand.Reader, f.c.Now)
	if e != nil {
		t.Fatal(e)
	}
	ca, e := clientcert.ParseCertificate(raw["ca.crt"])
	if e != nil {
		t.Fatal(e)
	}
	p := &peopleFixture{path: path, f: f, state: state, admin: &adminStoreAdapter{Store: state}, publish: &testAuthorityPublication{}, cluster: &testClusterPolicy{}, issuer: &testIssuer{Issuer: authority}}
	f.c.People = service.People{StateMetadata: state, Schedules: &testSchedules{}, Issues: state, Enrollment: state, Grants: state, Admin: state, Publication: p.publish, Cluster: p.cluster, Administration: &testAdministration{}, Issuer: p.issuer, Random: rand.Reader, CAPin: clientcert.Pin(ca), PublicURL: "https://ci.example/mcp", Backup: state, BackupOptions: stateport.Options{Protocol: stateport.DefaultProtocol(), Schema: store.Schema}}
	f.restart(t)
	return p
}

// This adapter forwards persistence to the real Store; it keeps no state facts.
type adminStoreAdapter struct {
	*store.Store
	Fail error
}

func (p *adminStoreAdapter) RecordAdministration(ctx context.Context, a stateport.Action, v model.AuditAction) error {
	if p.Fail != nil {
		return p.Fail
	}
	return p.Store.RecordAdministration(ctx, a, v)
}

type testAuthorityPublication struct {
	Fence    func(context.Context, func(context.Context) (stateport.AuthoritySnapshot, error)) error
	Snapshot stateport.AuthoritySnapshot
}

func (p *testAuthorityPublication) Publish(ctx context.Context, apply func(context.Context) (stateport.AuthoritySnapshot, error)) error {
	if p.Fence != nil {
		return p.Fence(ctx, apply)
	}
	v, e := apply(ctx)
	if e == nil {
		p.Snapshot = v.Clone()
	}
	return e
}

type testClusterPolicy struct {
	Validate func(context.Context, apicontract.Capability) error
	Lookup   func(context.Context, string, string) (bool, error)
}

func (p *testClusterPolicy) ValidateCapability(ctx context.Context, v apicontract.Capability) error {
	if p.Validate != nil {
		return p.Validate(ctx, v)
	}
	return nil
}
func (*testClusterPolicy) ServerNamespace(v string) bool {
	return v == "oberth" || v == "oberth-system"
}
func (p *testClusterPolicy) Namespaced(ctx context.Context, version, kind string) (bool, error) {
	if p.Lookup != nil {
		return p.Lookup(ctx, version, kind)
	}
	return kind != "ClusterRole", nil
}

type testIssuer struct {
	service.Issuer
	Identities []string
}

func (p *testIssuer) Issue(identity string, raw []byte, d time.Duration) ([]byte, *x509.Certificate, error) {
	p.Identities = append(p.Identities, identity)
	return p.Issuer.Issue(identity, raw, d)
}

type testAdministration struct {
	Prepare func(context.Context, apicontract.UpstreamAddArgs) (service.PreparedUpstream, error)
	Probe   func(context.Context, string, string) (apicontract.Upstream, error)
	Outbox  func(stateport.Action, []string, apicontract.ReceiveOutboxArgs) (apicontract.OutboxResult, error)
	Key     func(context.Context, apicontract.UpstreamProvideKeyArgs) (apicontract.Upstream, error)
}

func (p *testAdministration) PrepareUpstream(ctx context.Context, q apicontract.UpstreamAddArgs) (service.PreparedUpstream, error) {
	if p.Prepare != nil {
		return p.Prepare(ctx, q)
	}
	return service.PreparedUpstream{Binding: stateport.Upstream{Name: q.Name, Org: "team", Kind: "git", BaseURL: q.BaseURL, KeyName: q.Name}}, nil
}
func (p *testAdministration) PersistKey(ctx context.Context, q apicontract.UpstreamProvideKeyArgs) (apicontract.Upstream, error) {
	if p.Key != nil {
		return p.Key(ctx, q)
	}
	return apicontract.Upstream{Name: q.Name}, nil
}
func (*testAdministration) ProbeRepository(context.Context, stateport.Repository) error { return nil }
func (p *testAdministration) ProbeUpstream(ctx context.Context, name, repo string) (apicontract.Upstream, error) {
	if p.Probe != nil {
		return p.Probe(ctx, name, repo)
	}
	return apicontract.Upstream{Name: name}, nil
}
func (*testAdministration) PublicIdentity(_ context.Context, name string) (apicontract.PublicIdentity, error) {
	return apicontract.PublicIdentity{Name: name}, nil
}
func (*testAdministration) Status(context.Context, string) (apicontract.SystemStatus, error) {
	return apicontract.SystemStatus{Parts: []apicontract.PartStatus{}, Ready: true, DisplayReason: "ready"}, nil
}
func publicKeys(t *testing.T) (string, string) {
	t.Helper()
	pub, key, e := ed25519.GenerateKey(rand.Reader)
	if e != nil {
		t.Fatal(e)
	}
	request, e := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{Subject: pkix.Name{CommonName: "attacker/admin"}, DNSNames: []string{"wider.example"}}, key)
	if e != nil {
		t.Fatal(e)
	}
	sshKey, e := ssh.NewPublicKey(pub)
	if e != nil {
		t.Fatal(e)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: request})), string(ssh.MarshalAuthorizedKey(sshKey))
}

type closeWriter struct {
	WriteFunc func([]byte) (int, error)
	CloseFunc func() error
}

func (w closeWriter) Write(p []byte) (int, error) {
	if w.WriteFunc != nil {
		return w.WriteFunc(p)
	}
	return len(p), nil
}
func (w closeWriter) Close() error {
	if w.CloseFunc != nil {
		return w.CloseFunc()
	}
	return nil
}

type snapshotFunc func(context.Context, stateport.Options, io.Writer) (stateport.BackupReport, error)

func (f snapshotFunc) Snapshot(ctx context.Context, o stateport.Options, w io.Writer) (stateport.BackupReport, error) {
	return f(ctx, o, w)
}
func adminCaller() apicontract.Caller  { return apicontract.Caller{Actor: admin()} }
func workerCaller() apicontract.Caller { return apicontract.Caller{Actor: caller()} }

type testSchedules struct {
	Act  func(stateport.Action, service.ScheduleControl, bool) (apicontract.Receipt, error)
	Read func([]string) (apicontract.SchedulePage, error)
}

func (p *testSchedules) List(_ context.Context, scope []string) (apicontract.SchedulePage, error) {
	if p.Read != nil {
		return p.Read(scope)
	}
	return apicontract.SchedulePage{Schedules: []apicontract.Schedule{}}, nil
}
func (p *testSchedules) SetPaused(_ context.Context, a stateport.Action, c service.ScheduleControl, paused bool) (apicontract.Receipt, error) {
	if p.Act != nil {
		return p.Act(a, c, paused)
	}
	return apicontract.Receipt{ID: c.Entry}, nil
}
func (p *testSchedules) Trigger(ctx context.Context, a stateport.Action, c service.ScheduleControl) (apicontract.Receipt, error) {
	return p.SetPaused(ctx, a, c, false)
}

func (p *testAdministration) ReceiveOutbox(_ context.Context, a stateport.Action, scope []string, q apicontract.ReceiveOutboxArgs) (apicontract.OutboxResult, error) {
	if p.Outbox != nil {
		return p.Outbox(a, scope, q)
	}
	return apicontract.OutboxResult{Records: []apicontract.OutboxRecord{}}, nil
}
