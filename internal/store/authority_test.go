package store

import (
	"crypto/ed25519"
	"crypto/x509"
	"encoding/base64"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/identityname"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/publicerr"
	"github.com/oberthci/oberth/internal/stateport"
	"github.com/oberthci/oberth/internal/stepmodel"
	"github.com/oberthci/oberth/internal/workflowpolicy"
)

func publicPin(t *testing.T) string {
	t.Helper()
	key := ed25519.NewKeyFromSeed(make([]byte, 32))
	body, err := x509.MarshalPKIXPublicKey(key.Public())
	requireOK(t, err)
	return base64.StdEncoding.EncodeToString(body)
}

func TestEnrollmentIsSingleUseAndAtomic(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	invitation := stateport.Invitation{Public: model.Invitation{ID: "invite-one", Identity: "agent", ServerURL: "https://oberth.ci", CAPin: strings.Repeat("a", 64), Scope: []string{"origin"}, ExpiresAt: s.options.Clock().Add(time.Hour)}, Digest: strings.Repeat("b", 64)}
	requireOK(t, s.Invite(t.Context(), stateport.Action{Actor: "operator"}, invitation))
	input := stateport.Enrollment{Reservation: "enrollment-one", InvitationID: invitation.Public.ID, Digest: invitation.Digest, SPKI: publicPin(t), Key: stateport.Key{Fingerprint: "agent-key", Identity: "agent", Kind: "agent", ParkOnPush: true, Scope: []string{"origin"}}, Credential: stateport.Credential{Digest: strings.Repeat("c", 64), Fingerprint: "agent-key", ExpiresAt: s.options.Clock().Add(time.Hour)}}
	_, reserveErr := s.ReserveInvitation(t.Context(), stateport.InvitationReservation{ID: input.InvitationID, Digest: input.Digest, Token: input.Reservation, Until: s.options.Clock().Add(time.Minute)})
	requireOK(t, reserveErr)
	execute(t, s.writer, `CREATE TRIGGER reject_enrollment BEFORE INSERT ON audit BEGIN SELECT RAISE(ABORT,'audit failed'); END`)
	if err := s.Enroll(t.Context(), input); err == nil {
		t.Fatal("audit failure ignored")
	}
	execute(t, s.writer, `DROP TRIGGER reject_enrollment`)
	_, err := s.Authenticate(t.Context(), 0, input.Credential.Digest)
	requireCode(t, err, publicerr.Forbidden)
	requireOK(t, s.Enroll(t.Context(), input))
	requireCode(t, s.Enroll(t.Context(), input), publicerr.Forbidden)
	identity, err := s.AuthenticateCertificate(t.Context(), 1, input.SPKI)
	requireOK(t, err)
	if identity.Identity != "agent" || len(identity.Scope) != 1 {
		t.Fatal("enrollment scope")
	}
	requireOK(t, s.AddCertificatePin(t.Context(), stateport.Action{Actor: "operator"}, stateport.CertificateBinding{Fingerprint: input.Key.Fingerprint, SPKI: input.SPKI, ExpiresAt: s.options.Clock().Add(time.Hour)}))
	requireOK(t, s.RegisterKey(t.Context(), stateport.Action{Actor: "operator"}, stateport.Key{Fingerprint: "other", Identity: "other", Kind: "operator"}))
	requireCode(t, s.AddCertificatePin(t.Context(), stateport.Action{Actor: "operator"}, stateport.CertificateBinding{Fingerprint: "other", SPKI: input.SPKI, ExpiresAt: s.options.Clock().Add(time.Hour)}), publicerr.Conflict)
	requireOK(t, s.RevokeKey(t.Context(), stateport.Action{Actor: "operator"}, input.Key.Fingerprint))
	_, err = s.AuthenticateCertificate(t.Context(), 1, input.SPKI)
	requireCode(t, err, publicerr.Forbidden)
}

func TestRotationExpiryAndLastAdminGuard(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	key := stateport.Key{Fingerprint: "admin-key", Identity: "operator", Admin: true, Kind: "operator"}
	requireOK(t, s.RegisterKey(t.Context(), stateport.Action{Actor: "operator"}, key))
	requireCode(t, s.RevokeKey(t.Context(), stateport.Action{Actor: "operator"}, key.Fingerprint), publicerr.Conflict)
	requireCode(t, s.SetKeyScope(t.Context(), stateport.Action{Actor: "operator"}, key.Fingerprint, []string{"origin"}), publicerr.Conflict)
	old := stateport.Credential{Digest: strings.Repeat("a", 64), Fingerprint: key.Fingerprint, ExpiresAt: s.options.Clock().Add(time.Hour)}
	requireOK(t, s.AddCredential(t.Context(), stateport.Action{Actor: "operator"}, old))
	next := old
	next.Digest = strings.Repeat("b", 64)
	requireOK(t, s.RotateCredential(t.Context(), stateport.Action{Actor: "operator"}, CredentialRotation{PreviousDigest: old.Digest, Next: next}))
	_, err := s.Authenticate(t.Context(), 0, old.Digest)
	requireCode(t, err, publicerr.Forbidden)
	identity, err := s.Authenticate(t.Context(), 0, next.Digest)
	requireOK(t, err)
	keyRead, err := s.Key(t.Context(), key.Fingerprint)
	requireOK(t, err)
	if keyRead.Kind != "operator" || keyRead.ParkOnPush {
		t.Fatal("rotation changed key policy")
	}
	if !identity.Admin || len(identity.Scope) != 0 {
		t.Fatal("rotation changed authority")
	}
	requireCode(t, s.AddCredential(t.Context(), stateport.Action{Actor: "operator"}, stateport.Credential{Digest: strings.Repeat("c", 64), Fingerprint: key.Fingerprint, ExpiresAt: s.options.Clock()}), publicerr.InvalidInput)
	execute(t, s.writer, `UPDATE credentials SET expires='2026-11-16T12:00:00Z'`)
	_, err = s.Authenticate(t.Context(), 0, next.Digest)
	requireCode(t, err, publicerr.Forbidden)
	execute(t, s.writer, `UPDATE keys SET identity=''`)
	_, err = s.Authenticate(t.Context(), 0, next.Digest)
	requireCode(t, err, publicerr.Forbidden)
}

func TestExactGrantsClaimOwnershipAndPublicConfiguration(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	requireOK(t, s.RegisterUpstream(t.Context(), stateport.Action{Actor: "operator"}, stateport.Upstream{Name: "origin", Org: "team", Kind: "git", BaseURL: "https://oberth.ci", KeyName: "source"}))
	_, errSetup := s.RegisterRepository(t.Context(), stateport.Action{Actor: "operator"}, "origin/team/repo")
	requireOK(t, errSetup)
	grant := workflowpolicy.GrantKey{Key: identityname.Key{Repository: "origin/team/repo", File: ".oberth/build.yaml", Step: "*"}, Path: "public/value"}
	filter := stateport.CatalogFilter{Repositories: []string{grant.Repository}, Limit: 200}
	grants, err := s.SecretGrants(t.Context(), filter)
	requireOK(t, err)
	if len(grants) != 0 {
		t.Fatal("declaration created a grant", grants)
	}
	requireOK(t, s.SecretGrant(t.Context(), stateport.Action{Actor: "operator"}, grant, false))
	grants, err = s.SecretGrants(t.Context(), filter)
	requireOK(t, err)
	if len(grants) != 1 || grants[0].GrantKey != grant || grants[0].Revoked {
		t.Fatal("grant tuple changed", grants)
	}
	requireOK(t, s.SecretGrant(t.Context(), stateport.Action{Actor: "operator"}, grant, true))
	grants, err = s.SecretGrants(t.Context(), filter)
	requireOK(t, err)
	if len(grants) != 1 || grants[0].GrantKey != grant || !grants[0].Revoked {
		t.Fatal("revocation not fresh", grants)
	}
	owner := ClaimOwner{Claim: "cache", Repository: grant.Repository, Tier: "ci"}
	requireOK(t, s.GrantClaim(t.Context(), stateport.Action{Actor: "operator"}, owner))
	requireOK(t, s.GrantClaim(t.Context(), stateport.Action{Actor: "operator"}, owner))
	_, err = s.RegisterRepository(t.Context(), stateport.Action{Actor: "operator"}, "origin/team/other")
	requireOK(t, err)
	owner.Repository = "origin/team/other"
	requireCode(t, s.GrantClaim(t.Context(), stateport.Action{Actor: "operator"}, owner), publicerr.Conflict)
	requireOK(t, s.PauseSchedule(t.Context(), stateport.Action{Actor: "operator"}, SchedulePause{Repository: grant.Repository, Entry: "daily", Paused: true}))
	paused, err := s.SchedulePaused(t.Context(), grant.Repository, "daily")
	requireOK(t, err)
	if !paused {
		t.Fatal("pause lost")
	}
	requireOK(t, s.Capability(t.Context(), stateport.Action{Actor: "operator"}, CapabilityGrant{Repository: grant.Repository, JSON: []byte(`{"enabled":true}`)}))
	body, err := s.ReadCapability(t.Context(), grant.Repository)
	requireOK(t, err)
	if string(body) != `{"enabled":true}` {
		t.Fatal("capability changed")
	}
}

func TestHistoryScopeArchiveAndUnknownRepositories(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	r, input := accepted(t, s, "history-one")
	page, err := s.Runs(t.Context(), stateport.RunFilter{Repositories: []string{}, Limit: 200})
	requireOK(t, err)
	if len(page) != 0 {
		t.Fatal("scope widened")
	}
	page, err = s.Runs(t.Context(), stateport.RunFilter{Repositories: []string{r.Repo}, Limit: 200})
	requireOK(t, err)
	if len(page) != 1 {
		t.Fatal("scoped history missing")
	}
	requireOK(t, s.RegisterUpstream(t.Context(), stateport.Action{Actor: "operator"}, stateport.Upstream{Name: "other", Org: "another", Kind: "git", BaseURL: "https://oberth.ci", KeyName: "source"}))
	_, err = s.RegisterRepository(t.Context(), stateport.Action{Actor: "operator"}, "other/another/repo")
	requireOK(t, err)
	_, err = s.Repository(t.Context(), "repo", nil)
	requireCode(t, err, publicerr.Ambiguous)
	repo, err := s.Repository(t.Context(), r.Repo, nil)
	requireOK(t, err)
	requireCode(t, s.ArchiveRepository(t.Context(), stateport.Action{Actor: "operator"}, repo.ID), publicerr.Conflict)
	requireCode(t, s.RemoveRepository(t.Context(), stateport.Action{Actor: "operator"}, repo.ID), publicerr.Conflict)
	requireOK(t, s.Finish(t.Context(), stateport.Action{Actor: "observer"}, stateport.RunUpdate{ID: r.ID, Revision: r.Revision, Status: stepmodel.RunFailed, Restart: true}))
	requireOK(t, s.ArchiveRepository(t.Context(), stateport.Action{Actor: "operator"}, repo.ID))
	requireOK(t, acceptPlan(t, s, input, planCase{"archived-intake", input.Prepared.View().Plan}))
	for i := 0; i < 2; i++ {
		_, err = s.Events(t.Context(), stateport.EventFilter{Repositories: nil, After: 0, Limit: 500})
		requireOK(t, err)
	}
	_, err = s.Runs(t.Context(), stateport.RunFilter{Limit: 201})
	requireCode(t, err, publicerr.InvalidInput)
	if _, err = s.Runs(t.Context(), stateport.RunFilter{Limit: 200, Status: stepmodel.RunFailed}); err != nil {
		t.Fatal(err)
	}
}
