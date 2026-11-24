package service_test

import (
	"context"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/publicerr"
	"github.com/oberthci/oberth/internal/service"
	"github.com/oberthci/oberth/internal/stateport"
)

func TestInvitationRejectsAliasBeforeCreation(t *testing.T) {
	t.Parallel()
	p := people(t)
	p.f.c.People.PublicURL = "https://ci.example:34443/mcp"
	p.f.c.People.SSHURL = "ssh://git@ci.example:30022"
	store := &countedInvitations{EnrollmentStore: p.state}
	p.f.c.People.Enrollment = store
	p.f.restart(t)
	q := apicontract.UplinkInviteArgs{Scope: []string{"*"}, Identity: "invited", ServerURL: "https://oberth", SecretDigest: digestText(strings.Repeat("s", 48))}
	for _, address := range []string{"https://oberth", "https://oberth.oberth.svc", "https://ci.example./mcp", "https://other.example/mcp", "https://ci.example:34444/mcp", "https://ci.example:34443/other"} {
		q.ServerURL = address
		before := p.f.seq.Load()
		call, err := p.f.s.PrepareUplinkInvite(t.Context(), adminCaller(), q)
		if err == nil || call != nil || store.created != 0 || p.f.seq.Load() != before {
			t.Fatal("alias allocated or prepared an invitation", address, err, store.created)
		}
		problem := publicerr.Classify(err)
		if problem.Code() != publicerr.InvalidInput || !strings.Contains(problem.Detail, p.f.c.People.PublicURL) || problem.Remedy() != "Correct server_url in the selected profile's installation.json, then run oberth invite <upstream>." {
			t.Fatal("alias refusal omitted the public address", problem)
		}
	}
	q.ServerURL = "https://CI.EXAMPLE:34443/mcp"
	assertPublicInvitationEnrolls(t, p, q)
	if store.created != 1 {
		t.Fatal("public invitation was not created exactly once", store.created)
	}
}

func assertPublicInvitationEnrolls(t *testing.T, p *peopleFixture, q apicontract.UplinkInviteArgs) {
	t.Helper()
	call, err := p.f.s.PrepareUplinkInvite(t.Context(), adminCaller(), q)
	invitation, err := execute(t.Context(), call, err)
	if err != nil || invitation.ServerURL != q.ServerURL {
		t.Fatal("public host could not create an invitation", invitation, err)
	}
	csr, key := publicKeys(t)
	joined, err := p.f.s.Redeem(t.Context(), invitation.ID, model.InviteRedemption{Secret: strings.Repeat("s", 48), ServerURL: invitation.ServerURL, CAPin: invitation.CAPin, CSR: csr, SSHPublicKey: key})
	if err != nil || joined.Identity != q.Identity || joined.SSHURL != p.f.c.People.SSHURL {
		t.Fatal("public invitation could not enroll", joined, err)
	}
}

func TestInvitationLongPublicAddressKeepsBoundedRemedy(t *testing.T) {
	t.Parallel()
	p := people(t)
	address := "https://ci.example/" + strings.Repeat("path/", 50)
	p.f.c.People.PublicURL = address
	p.f.restart(t)
	q := apicontract.UplinkInviteArgs{Scope: []string{"*"}, Identity: "invited", ServerURL: "https://oberth", SecretDigest: digestText(strings.Repeat("s", 48))}
	call, err := p.f.s.PrepareUplinkInvite(t.Context(), adminCaller(), q)
	if err == nil || call != nil {
		t.Fatal("alias prepared an invitation", err)
	}
	problem := publicerr.Classify(err)
	if publicerr.CheckMessage(problem.Reason(), 80) != nil || publicerr.CheckMessage(problem.Remedy(), 120) != nil || problem.Remedy() != "Correct server_url in the selected profile's installation.json, then run oberth invite <upstream>." || !strings.Contains(problem.Detail, address) {
		t.Fatal("public-address remedy is unbounded or lost its target", problem)
	}
}

type countedInvitations struct {
	service.EnrollmentStore
	created  int
	reserved int
}

func TestInvitationHostComparisonUsesASCII(t *testing.T) {
	t.Parallel()
	for _, pair := range [][2]string{
		{"https://kube.local", "https://\u212Aube.local"},
		{"https://host.local", "https://ho\u017Ft.local"},
		{"https://\u212Aube.local", "https://kube.local"},
	} {
		p := people(t)
		p.f.c.People.PublicURL = pair[0]
		p.f.restart(t)
		q := apicontract.UplinkInviteArgs{Scope: []string{"*"}, Identity: "invited", ServerURL: pair[1], SecretDigest: digestText(strings.Repeat("s", 48))}
		call, err := p.f.s.PrepareUplinkInvite(t.Context(), adminCaller(), q)
		if err == nil || call != nil {
			t.Fatal("Unicode folding substituted the public host", pair, err)
		}
	}
}

func (s *countedInvitations) Invite(ctx context.Context, action stateport.Action, invitation stateport.Invitation) error {
	s.created++
	return s.EnrollmentStore.Invite(ctx, action, invitation)
}

func TestRedeemRefusesChangedPublicAddressBeforeSpendingInvitation(t *testing.T) {
	for _, address := range []string{"https://other.example/mcp", "https://ci.example:444/mcp", "https://ci.example/other", "http://ci.example/mcp"} {
		t.Run(address, func(t *testing.T) {
			p := people(t)
			invitation, q := reviewInvitation(t, p)
			p.f.c.People.PublicURL = address
			store := &countedInvitations{EnrollmentStore: p.state}
			p.f.c.People.Enrollment = store
			p.f.restart(t)
			_, err := p.f.s.Redeem(t.Context(), invitation.ID, q)
			problem := publicerr.Classify(err)
			if err == nil || problem.Reason() != "The invitation address has changed." || problem.Remedy() != "Ask the administrator to run oberth invite for a new invitation." || len(p.issuer.Identities) != 0 || store.reserved != 0 {
				t.Fatal("changed address reached issuance or omitted recovery", err)
			}
			if publicerr.CheckMessage(problem.Reason(), 80) != nil || publicerr.CheckMessage(problem.Remedy(), 120) != nil {
				t.Fatal("incomplete address refusal")
			}
			p.f.c.People.PublicURL = "https://CI.EXAMPLE:443/mcp/"
			p.f.restart(t)
			if _, err = p.f.s.Redeem(t.Context(), invitation.ID, q); err != nil {
				t.Fatal("immediate retry after correcting the address failed", err)
			}
		})
	}
}

func (s *countedInvitations) ReserveInvitation(ctx context.Context, reservation stateport.InvitationReservation) (model.Invitation, error) {
	s.reserved++
	return s.EnrollmentStore.ReserveInvitation(ctx, reservation)
}
