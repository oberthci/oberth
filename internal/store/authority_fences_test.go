package store

import (
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/publicerr"
	"github.com/oberthci/oberth/internal/stateport"
)

func TestConfigurationRefusalsAndRevokedReads(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	repo := atomicRepository(t, s)
	a := stateport.Action{Actor: "operator"}
	requireCode(t, s.Capability(t.Context(), a, CapabilityGrant{Repository: repo.Canonical, JSON: []byte(`{`)}), publicerr.InvalidInput)
	requireCode(t, s.GrantClaim(t.Context(), a, ClaimOwner{Repository: repo.Canonical, Claim: "cache", Tier: "other"}), publicerr.InvalidInput)
	requireCode(t, s.PauseSchedule(t.Context(), a, SchedulePause{Repository: repo.Canonical}), publicerr.InvalidInput)
	requireCode(t, s.Capability(t.Context(), a, CapabilityGrant{Repository: repo.Canonical}), publicerr.InvalidInput)
	cap := CapabilityGrant{Repository: repo.Canonical, JSON: []byte(`{"enabled":true}`)}
	requireOK(t, s.Capability(t.Context(), a, cap))
	cap.Revoke = true
	requireOK(t, s.Capability(t.Context(), a, cap))
	_, err := s.ReadCapability(t.Context(), repo.Canonical)
	requireCode(t, err, publicerr.NotFound)
	pause := SchedulePause{Repository: repo.Canonical, Entry: "daily", Paused: true}
	requireOK(t, s.PauseSchedule(t.Context(), a, pause))
	pause.Paused = false
	requireOK(t, s.PauseSchedule(t.Context(), a, pause))
	paused, err := s.SchedulePaused(t.Context(), repo.Canonical, "daily")
	requireOK(t, err)
	if paused {
		t.Fatal("resumed schedule still paused")
	}
}

func TestLastAdminRequiresAnotherUsableCredential(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	a := stateport.Action{Actor: "operator"}
	for _, id := range []string{"first", "expired"} {
		requireOK(t, s.RegisterKey(t.Context(), a, stateport.Key{Fingerprint: id, Identity: id, Kind: "operator", Admin: true}))
	}
	requireOK(t, s.AddCredential(t.Context(), a, stateport.Credential{Fingerprint: "expired", Digest: strings.Repeat("a", 64), ExpiresAt: s.options.Clock().Add(time.Hour)}))
	execute(t, s.writer, `UPDATE credentials SET expires='2000-01-01T00:00:00Z'`)
	requireCode(t, s.RevokeKey(t.Context(), a, "first"), publicerr.Conflict)
}

func TestInvitationExpiryScopeAndReservationFences(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"scope", "expired", "reservation"} {
		t.Run(mode, func(t *testing.T) {
			s := testStore(t)
			invite, input := atomicEnrollment(t, s)
			a := stateport.Action{Actor: "operator"}
			requireOK(t, s.Invite(t.Context(), a, invite))
			reservation := stateport.InvitationReservation{ID: input.InvitationID, Digest: input.Digest, Token: input.Reservation, Until: s.options.Clock().Add(time.Minute)}
			_, err := s.ReserveInvitation(t.Context(), reservation)
			requireOK(t, err)
			switch mode {
			case "scope":
				input.Key.Scope = []string{"other"}
			case "expired":
				invite.Public.ExpiresAt = s.options.Clock()
				_, err = s.writer.ExecContext(t.Context(), `UPDATE invitations SET value=?`, encode(invite.Public))
				requireOK(t, err)
			case "reservation":
				reservation.Token = "other"
				_, err = s.ReserveInvitation(t.Context(), reservation)
				requireCode(t, err, publicerr.Conflict)
				input.Reservation = "other"
			}
			requireCode(t, s.Enroll(t.Context(), input), publicerr.Forbidden)
			_, err = s.Key(t.Context(), input.Key.Fingerprint)
			requireCode(t, err, publicerr.NotFound)
		})
	}
}

func TestUpstreamReferencesAndForeignOrganization(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	r := atomicRepository(t, s)
	a := stateport.Action{Actor: "operator"}
	requireCode(t, s.RemoveUpstream(t.Context(), a, r.Upstream), publicerr.Conflict)
	_, err := s.RegisterRepository(t.Context(), a, "origin/foreign/repo")
	requireCode(t, err, publicerr.Conflict)
	for _, operation := range []func() error{
		func() error { return s.ArchiveRepository(t.Context(), a, 999) }, func() error { return s.UnarchiveRepository(t.Context(), a, 999) }, func() error { return s.RemoveRepository(t.Context(), a, 999) },
	} {
		requireCode(t, operation(), publicerr.NotFound)
	}
	_, err = s.CreateIssue(t.Context(), a, model.Issue{Kind: model.IssueManual, Title: "history", Repo: r.Canonical})
	requireOK(t, err)
	requireCode(t, s.RemoveRepository(t.Context(), a, r.ID), publicerr.Conflict)
}

func TestRevokedKeyCannotAcquireANewCredential(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	key := atomicKey(t, s)
	a := stateport.Action{Actor: "operator"}
	requireOK(t, s.RevokeKey(t.Context(), a, key.Fingerprint))
	requireCode(t, s.AddCredential(t.Context(), a, stateport.Credential{Fingerprint: key.Fingerprint, Digest: strings.Repeat("f", 64), ExpiresAt: s.options.Clock().Add(time.Hour)}), publicerr.Forbidden)
}
