package service_test

import (
	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/publicerr"
	"github.com/oberthci/oberth/internal/stateport"
	"strings"
	"testing"
	"time"
)

func registerReviewKey(t *testing.T, p *peopleFixture, identity string) apicontract.Identity {
	t.Helper()
	_, key := publicKeys(t)
	call, e := p.f.s.PrepareUplinkAdd(t.Context(), adminCaller(), apicontract.UplinkAddArgs{Scope: []string{"*"}, Identity: identity, SshPublicKey: key})
	v, e := execute(t.Context(), call, e)
	if e != nil {
		t.Fatal(e)
	}
	return v
}
func TestCertificatePreparationRejectsRevokedAndForeignKeys(t *testing.T) {
	t.Parallel()
	p := people(t)
	v := registerReviewKey(t, p, "registered")
	csr, _ := publicKeys(t)
	_, e := p.f.s.PrepareUplinkCert(t.Context(), adminCaller(), apicontract.UplinkCertArgs{Identity: "other", Fingerprint: v.Fingerprint, CSR: csr})
	requireCode(t, e, publicerr.NotFound)
	if e = p.state.RevokeKey(t.Context(), stateport.Action{Actor: admin().Identity}, v.Fingerprint); e != nil {
		t.Fatal(e)
	}
	for _, fp := range []string{v.Fingerprint, ""} {
		call, e := p.f.s.PrepareUplinkCert(t.Context(), adminCaller(), apicontract.UplinkCertArgs{Identity: v.Identity, Fingerprint: fp, CSR: csr})
		requireCode(t, e, publicerr.NotFound)
		if call != nil {
			t.Fatal("revoked certificate prepared", fp)
		}
	}
	if len(p.issuer.Identities) != 0 {
		t.Fatal("refused certificate reached issuer")
	}
}
func reviewInvitation(t *testing.T, p *peopleFixture) (model.Invitation, model.InviteRedemption) {
	t.Helper()
	secret := strings.Repeat("s", 48)
	csr, key := publicKeys(t)
	call, e := p.f.s.PrepareUplinkInvite(t.Context(), adminCaller(), apicontract.UplinkInviteArgs{Scope: []string{"*"}, Identity: "invited", ServerURL: "https://ci.example/mcp", SecretDigest: digestText(secret)})
	v, e := execute(t.Context(), call, e)
	if e != nil {
		t.Fatal(e)
	}
	return v, model.InviteRedemption{Secret: secret, ServerURL: v.ServerURL, CAPin: v.CAPin, CSR: csr, SSHPublicKey: key}
}
func TestInvitationRequiresItsServerAndAuthority(t *testing.T) {
	t.Parallel()
	for _, field := range []string{"server", "pin"} {
		t.Run(field, func(t *testing.T) {
			p := people(t)
			v, q := reviewInvitation(t, p)
			if field == "server" {
				q.ServerURL = "https://other.example/mcp"
			} else {
				q.CAPin = "sha256:other"
			}
			_, e := p.f.s.Redeem(t.Context(), v.ID, q)
			requireCode(t, e, publicerr.Forbidden)
			if len(p.issuer.Identities) != 0 {
				t.Fatal("foreign invitation reached issuer")
			}
		})
	}
}
func TestInvitationRequiresCSRAndCanonicalDigest(t *testing.T) {
	t.Parallel()
	p := people(t)
	v, q := reviewInvitation(t, p)
	q.CSR = ""
	_, e := p.f.s.Redeem(t.Context(), v.ID, q)
	requireField(t, e, "csr")
	for _, digest := range []string{strings.Repeat("A", 64), strings.Repeat("a", 62)} {
		call, e := p.f.s.PrepareUplinkInvite(t.Context(), adminCaller(), apicontract.UplinkInviteArgs{Scope: []string{"*"}, Identity: "invited", ServerURL: "https://ci.example/mcp", SecretDigest: digest})
		requireField(t, e, "secret_digest")
		if call != nil {
			t.Fatal("noncanonical digest prepared")
		}
	}
}
func TestUplinkListKeepsActualRevocationTime(t *testing.T) {
	t.Parallel()
	p := people(t)
	v := registerReviewKey(t, p, "revoked")
	revoked := p.f.c.Now()
	if e := p.state.RevokeKey(t.Context(), stateport.Action{Actor: admin().Identity}, v.Fingerprint); e != nil {
		t.Fatal(e)
	}
	p.f.offset.Add(int64(5 * time.Hour))
	call, e := p.f.s.PrepareUplinkList(t.Context(), adminCaller(), apicontract.UplinkListArgs{})
	page, e := execute(t.Context(), call, e)
	if e != nil || len(page.Uplinks) != 1 || page.Uplinks[0].RevokedAt == nil || !page.Uplinks[0].RevokedAt.Equal(revoked) {
		t.Fatal("revocation time fabricated", page, e)
	}
}
func TestRotationCannotChangeAuthorityOrExecutionPolicy(t *testing.T) {
	t.Parallel()
	for _, field := range []string{"admin", "kind_and_push_policy", "scope"} {
		t.Run(field, func(t *testing.T) {
			p := people(t)
			old := registerReviewKey(t, p, "rotate")
			next := stateport.Key{Identity: old.Identity, Fingerprint: "replacement", Kind: "agent", ParkOnPush: true, Scope: []string{}}
			switch field {
			case "admin":
				next.Admin = true
			case "kind_and_push_policy":
				next.Kind = "operator"
				next.ParkOnPush = false
			case "scope":
				next.Scope = []string{"origin"}
			}
			if e := p.state.RegisterKey(t.Context(), stateport.Action{Actor: admin().Identity}, next); e != nil {
				t.Fatal(e)
			}
			call, e := p.f.s.PrepareUplinkRotate(t.Context(), adminCaller(), apicontract.UplinkRotateArgs{Identity: old.Identity, Fingerprint: old.Fingerprint, ReplacementFingerprint: next.Fingerprint})
			requireCode(t, e, publicerr.Forbidden)
			if call != nil {
				t.Fatal("authority-changing rotation prepared")
			}
		})
	}
}
func TestScopeChangeRequiresExplicitScope(t *testing.T) {
	t.Parallel()
	p := people(t)
	v := registerReviewKey(t, p, "scope")
	_, e := p.f.s.PrepareUplinkScope(t.Context(), adminCaller(), apicontract.UplinkScopeArgs{Identity: v.Identity, Fingerprint: v.Fingerprint})
	requireField(t, e, "scope")
	nullMember := []*string{nil}
	_, e = p.f.s.PrepareUplinkScope(t.Context(), adminCaller(), apicontract.UplinkScopeArgs{Identity: v.Identity, Fingerprint: v.Fingerprint, Scope: &nullMember})
	requireField(t, e, "scope")
	empty := []*string{}
	call, e := p.f.s.PrepareUplinkScope(t.Context(), adminCaller(), apicontract.UplinkScopeArgs{Identity: v.Identity, Fingerprint: v.Fingerprint, Scope: &empty})
	if e == nil || call != nil {
		t.Fatal("empty scope widened repository access")
	}
	all := "*"
	explicit := []*string{&all}
	call, e = p.f.s.PrepareUplinkScope(t.Context(), adminCaller(), apicontract.UplinkScopeArgs{Identity: v.Identity, Fingerprint: v.Fingerprint, Scope: &explicit})
	got, e := execute(t.Context(), call, e)
	if e != nil || got.Scope == nil || len(got.Scope) != 0 {
		t.Fatal("explicit all-repositories scope refused", got, e)
	}
}
