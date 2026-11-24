package service_test

import (
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/publicerr"
)

func TestInvitationRequiresExplicitScopeBeforeAllocation(t *testing.T) {
	for _, scope := range [][]string{nil, {}, {"*", "origin"}} {
		p := people(t)
		store := &countedInvitations{EnrollmentStore: p.state}
		p.f.c.People.Enrollment = store
		p.f.restart(t)
		before := p.f.seq.Load()
		q := apicontract.UplinkInviteArgs{Identity: "invited", Scope: scope, ServerURL: "https://ci.example/mcp", SecretDigest: digestText(strings.Repeat("s", 48))}
		call, err := p.f.s.PrepareUplinkInvite(t.Context(), adminCaller(), q)
		if err == nil || call != nil || store.created != 0 || p.f.seq.Load() != before {
			t.Fatal("implicit invitation allocated repository authority", err)
		}
		requireCode(t, err, publicerr.InvalidInput)
	}
}

func TestInvitationExplicitScopeIsFrozenBeforeRedemption(t *testing.T) {
	for _, scope := range [][]string{{"origin"}, {"*"}} {
		p := people(t)
		q := apicontract.UplinkInviteArgs{Identity: "invited", Scope: scope, ServerURL: "https://ci.example/mcp", SecretDigest: digestText(strings.Repeat("s", 48))}
		call, err := p.f.s.PrepareUplinkInvite(t.Context(), adminCaller(), q)
		want := scope[0]
		scope[0] = "unexpected"
		got, err := execute(t.Context(), call, err)
		if err != nil || (want == "*" && len(got.Scope) != 0) || (want != "*" && (len(got.Scope) != 1 || got.Scope[0] != want)) {
			t.Fatal("prepared invitation scope changed", got.Scope, err)
		}
	}
}
