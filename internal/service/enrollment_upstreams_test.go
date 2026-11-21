package service_test

import (
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/stateport"
)

func TestEnrollmentRejectsUnregisteredUpstreamScopes(t *testing.T) {
	for _, operation := range []string{"add", "invite", "scope"} {
		t.Run(operation, func(t *testing.T) { checkUnknownEnrollmentScope(t, operation) })
	}
}

func checkUnknownEnrollmentScope(t *testing.T, operation string) {
	t.Helper()
	p := people(t)
	csr, key := publicKeys(t)
	scope := []string{"zeta", "origin", "alpha", "zeta"}
	var err error
	switch operation {
	case "add":
		call, e := p.f.s.PrepareUplinkAdd(t.Context(), adminCaller(), apicontract.UplinkAddArgs{Identity: "viewer", Scope: scope, SshPublicKey: key, CSR: csr})
		_, err = execute(t.Context(), call, e)
	case "invite":
		call, e := p.f.s.PrepareUplinkInvite(t.Context(), adminCaller(), apicontract.UplinkInviteArgs{Identity: "viewer", Scope: scope, ServerURL: "https://ci.example/mcp", SecretDigest: digestText(strings.Repeat("s", 48))})
		_, err = execute(t.Context(), call, e)
	case "scope":
		existing := registerReviewKey(t, p, "viewer")
		names := []*string{new("zeta"), new("origin"), new("alpha")}
		call, e := p.f.s.PrepareUplinkScope(t.Context(), adminCaller(), apicontract.UplinkScopeArgs{Identity: existing.Identity, Fingerprint: existing.Fingerprint, Scope: &names})
		_, err = execute(t.Context(), call, e)
		stored, e := p.state.Key(t.Context(), existing.Fingerprint)
		if e != nil || len(stored.Scope) != 0 {
			t.Fatal("refused scope update changed existing authority", stored.Scope, e)
		}
	}
	assertUnknownUpstreams(t, err, `Unknown upstreams: "alpha", "zeta"`)
	assertRejectedScopeUnchanged(t, p, operation)
	checkRegisteredScopeSucceeds(t, p, operation)
}

func assertRejectedScopeUnchanged(t *testing.T, p *peopleFixture, operation string) {
	t.Helper()
	if len(p.issuer.Identities) != 0 {
		t.Fatal("unknown scope reached certificate issuance")
	}
	if operation != "scope" {
		rows, e := p.state.IdentityKeys(t.Context(), "viewer")
		if e != nil || len(rows) != 0 {
			t.Fatal("unknown scope activated an identity", rows, e)
		}
	}
}

func checkRegisteredScopeSucceeds(t *testing.T, p *peopleFixture, operation string) {
	t.Helper()
	_, key := publicKeys(t)
	scope := []string{"zeta", "origin", "alpha"}
	registerIssueUpstream(t, p, "alpha")
	registerIssueUpstream(t, p, "zeta")
	switch operation {
	case "add":
		call, e := p.f.s.PrepareUplinkAdd(t.Context(), adminCaller(), apicontract.UplinkAddArgs{Identity: "viewer", Scope: scope, SshPublicKey: key})
		if _, e = execute(t.Context(), call, e); e != nil {
			t.Fatal(e)
		}
	case "invite":
		call, e := p.f.s.PrepareUplinkInvite(t.Context(), adminCaller(), apicontract.UplinkInviteArgs{Identity: "viewer", Scope: scope, ServerURL: "https://ci.example/mcp", SecretDigest: digestText(strings.Repeat("s", 48))})
		if _, e = execute(t.Context(), call, e); e != nil {
			t.Fatal(e)
		}
	case "scope":
		names := []*string{new("zeta"), new("origin"), new("alpha")}
		call, e := p.f.s.PrepareUplinkScope(t.Context(), adminCaller(), apicontract.UplinkScopeArgs{Identity: "viewer", Scope: &names})
		if _, e = execute(t.Context(), call, e); e != nil {
			t.Fatal(e)
		}
	}
}

func TestInvitationRedemptionRejectsRemovedUpstreamBeforeIssuance(t *testing.T) {
	p := people(t)
	registerIssueUpstream(t, p, "departed")
	secret := strings.Repeat("s", 48)
	call, err := p.f.s.PrepareUplinkInvite(t.Context(), adminCaller(), apicontract.UplinkInviteArgs{Identity: "viewer", Scope: []string{"departed"}, ServerURL: "https://ci.example/mcp", SecretDigest: digestText(secret)})
	invitation, err := execute(t.Context(), call, err)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.state.RemoveUpstream(t.Context(), stateport.Action{Actor: admin().Identity}, "departed"); err != nil {
		t.Fatal(err)
	}
	csr, key := publicKeys(t)
	_, err = p.f.s.Redeem(t.Context(), invitation.ID, model.InviteRedemption{Secret: secret, ServerURL: invitation.ServerURL, CAPin: invitation.CAPin, CSR: csr, SSHPublicKey: key})
	assertUnknownUpstreams(t, err, `Unknown upstreams: "departed"`)
	assertRejectedScopeUnchanged(t, p, "invite")
}
