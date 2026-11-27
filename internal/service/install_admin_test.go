package service_test

import (
	"context"
	"encoding/base64"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/actor"
	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/publicerr"
	"github.com/oberthci/oberth/internal/service"
	"github.com/oberthci/oberth/internal/stateport"
)

type installCalls struct {
	service.EnrollmentStore
	calls   int
	failure error
}

func (p *installCalls) InstallAdmin(ctx context.Context, a stateport.Action, v stateport.IdentityRegistration) error {
	p.calls++
	if p.failure != nil {
		return p.failure
	}
	return p.EnrollmentStore.InstallAdmin(ctx, a, v)
}
func installCertificate(t *testing.T, p *peopleFixture) ([]byte, string) {
	t.Helper()
	csr, pub := publicKeys(t)
	_, cert, err := p.issuer.Issue("install-admin", []byte(csr), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	return []byte(pub), base64.StdEncoding.EncodeToString(cert.RawSubjectPublicKeyInfo)
}
func installedActor(t *testing.T, p *peopleFixture, pin string) actor.Actor {
	t.Helper()
	a, err := p.state.AuthenticateCertificate(t.Context(), 0, pin)
	if err != nil || !a.Admin || len(a.Scope) != 0 || a.Identity != "install-admin" {
		t.Fatal("install key lacks ordinary administrator authority", a, err)
	}
	return a
}
func TestInstallAdminUsesOneAuditedOrdinaryIdentity(t *testing.T) {
	t.Parallel()
	p := people(t)
	certificateKey, pin := installCertificate(t, p)
	_, push := publicKeys(t)
	calls := &installCalls{EnrollmentStore: p.state}
	p.f.c.People.Enrollment = calls
	p.f.restart(t)
	if err := p.f.s.InstallAdmin(t.Context(), certificateKey, []byte(push)); err != nil {
		t.Fatal(err)
	}
	a := installedActor(t, p, pin)
	p.f.store.Actors[a.Fingerprint] = a
	call, err := p.f.s.Bindings().AdminReadiness.Handler.Prepare(t.Context(), apicontract.Caller{Actor: a}, apicontract.AdminReadinessArgs{})
	result, err := execute(t.Context(), call, err)
	if err != nil || !result.Ready || calls.calls != 1 {
		t.Fatal("install identity could not use ordinary admin handler", result, calls.calls, err)
	}
	audits := installedAudit(t, p, 1)
	events, err := p.state.Events(t.Context(), stateport.EventFilter{Limit: 200})
	if err != nil {
		t.Fatal(err)
	}
	changes := observeChanges(t, p.state)
	if err = p.f.s.InstallAdmin(t.Context(), certificateKey, []byte(push)); err != nil {
		t.Fatal(err)
	}
	after, _, err := p.state.Audit(t.Context(), 0, 200)
	if err != nil || len(after) != len(audits) {
		t.Fatal("same installed binding repeated fact", after, err)
	}
	assertUnchangedInstallEvents(t, p, events)
	select {
	case <-changes:
		t.Fatal("same installed binding woke observers")
	default:
	}
}
func TestInstallAdminReplacesCertificateAndPushAuthority(t *testing.T) {
	t.Parallel()
	for _, which := range []string{"certificate", "both", "push"} {
		t.Run(which, func(t *testing.T) {
			checkInstallRotation(t, which)
		})
	}
}
func TestInstallAdminRefusesMalformedKeysWithoutWriting(t *testing.T) {
	t.Parallel()
	p := people(t)
	_, key := publicKeys(t)
	spy := &installCalls{EnrollmentStore: p.state}
	p.f.c.People.Enrollment = spy
	p.f.restart(t)
	for _, bad := range []string{"", "private input", key + key, `command="true" ` + key} {
		for _, keys := range [][2][]byte{{[]byte(bad), []byte(key)}, {[]byte(key), []byte(bad)}} {
			requireCode(t, p.f.s.InstallAdmin(t.Context(), keys[0], keys[1]), publicerr.InvalidInput)
		}
	}
	if spy.calls != 0 {
		t.Fatal("malformed key reached store", spy.calls)
	}
}
func TestInstallAdminPublicKeyBoundAndStoreFailure(t *testing.T) {
	t.Parallel()
	p := people(t)
	_, key := publicKeys(t)
	key = strings.TrimSpace(key)
	bounded := []byte(key + strings.Repeat(" ", 64<<10-len(key)))
	spy := &installCalls{EnrollmentStore: p.state, failure: errors.New("install transaction refused")}
	p.f.c.People.Enrollment = spy
	p.f.restart(t)
	for _, pair := range [][2][]byte{{bounded, []byte(key)}, {[]byte(key), bounded}} {
		if err := p.f.s.InstallAdmin(t.Context(), pair[0], pair[1]); !errors.Is(err, spy.failure) {
			t.Fatal("maximum key refused or store failure hidden", err)
		}
	}
	if spy.calls != 2 {
		t.Fatal("multiple writes per install call", spy.calls)
	}
	tooLarge := append(append([]byte{}, bounded...), ' ')
	for _, pair := range [][2][]byte{{tooLarge, []byte(key)}, {[]byte(key), tooLarge}} {
		requireCode(t, p.f.s.InstallAdmin(t.Context(), pair[0], pair[1]), publicerr.InvalidInput)
	}
	if spy.calls != 2 {
		t.Fatal("oversized key reached store")
	}
}
func TestInstallAdminHonorsServiceLifetime(t *testing.T) {
	t.Parallel()
	f := setup(t)
	requireCode(t, f.s.InstallAdmin(t.Context(), nil, nil), publicerr.Unavailable)
	p := people(t)
	_, key := publicKeys(t)
	spy := &installCalls{EnrollmentStore: p.state}
	p.f.c.People.Enrollment = spy
	p.f.restart(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := p.f.s.InstallAdmin(ctx, []byte(key), []byte(key)); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if err := p.f.s.Shutdown(t.Context()); err != nil {
		t.Fatal(err)
	}
	requireCode(t, p.f.s.InstallAdmin(t.Context(), []byte(key), []byte(key)), publicerr.Unavailable)
	if spy.calls != 0 {
		t.Fatal("stopped startup reached store", spy.calls)
	}
}

func installedAudit(t *testing.T, p *peopleFixture, want int) []model.AuditAction {
	t.Helper()
	audits, _, err := p.state.Audit(t.Context(), 0, 200)
	if err != nil {
		t.Fatal(err)
	}
	installs := 0
	for _, audit := range audits {
		if audit.Action == "identity.install" {
			installs++
			if audit.Actor != "installation" {
				t.Fatal("startup actor lost", audit)
			}
		}
	}
	events, err := p.state.Events(t.Context(), stateport.EventFilter{Limit: 200})
	if err != nil || installs != want || len(events.Events) != 0 {
		t.Fatal("install audit missing or unexpected event", installs, events, err)
	}
	return audits
}

func checkInstallRotation(t *testing.T, which string) {
	t.Helper()
	p := people(t)
	adminKey, oldPin := installCertificate(t, p)
	_, push := publicKeys(t)
	if err := p.f.s.InstallAdmin(t.Context(), adminKey, []byte(push)); err != nil {
		t.Fatal(err)
	}
	old := installedActor(t, p, oldPin)
	credential := stateport.Credential{Fingerprint: old.Fingerprint, Digest: strings.Repeat("a", 64), ExpiresAt: at().Add(time.Hour)}
	if err := p.state.AddCredential(t.Context(), stateport.Action{Actor: old.Identity}, credential); err != nil {
		t.Fatal(err)
	}
	nextPin := oldPin
	if which != "push" {
		adminKey, nextPin = installCertificate(t, p)
	}
	if which != "certificate" {
		_, push = publicKeys(t)
	}
	if err := p.f.s.InstallAdmin(t.Context(), adminKey, []byte(push)); err != nil {
		t.Fatal(err)
	}
	installedAudit(t, p, 2)
	next := installedActor(t, p, nextPin)
	if which != "push" {
		_, err := p.state.AuthenticateCertificate(t.Context(), 0, oldPin)
		requireCode(t, err, publicerr.Forbidden)
	}
	if which != "certificate" {
		checkInstallPushRetired(t, p, old, next)
	}
	_, err := p.state.Authenticate(t.Context(), 0, credential.Digest)
	requireCode(t, err, publicerr.Forbidden)
}

func checkInstallPushRetired(t *testing.T, p *peopleFixture, old, next actor.Actor) {
	t.Helper()
	key, err := p.state.Key(t.Context(), old.Fingerprint)
	if err != nil || !key.Revoked || next.Fingerprint == old.Fingerprint {
		t.Fatal("old push key survived", key, err)
	}
}

func assertUnchangedInstallEvents(t *testing.T, p *peopleFixture, events model.EventPage) {
	t.Helper()
	repeated, err := p.state.Events(t.Context(), stateport.EventFilter{Limit: 200})
	if err != nil || repeated.LatestID != events.LatestID || len(repeated.Events) != len(events.Events) {
		t.Fatal("same installed binding repeated event", repeated, err)
	}
}
