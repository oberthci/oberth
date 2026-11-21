package store

import (
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/identityname"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/stateport"
	"github.com/oberthci/oberth/internal/workflowpolicy"
)

func TestAtomicAuthorityFacts(t *testing.T) {
	t.Parallel()
	checkAtomicFacts(t, []atomicFact{
		{"key.register", func(t *testing.T, s *Store) func(stateport.Action) error {
			return func(a stateport.Action) error {
				return s.RegisterKey(t.Context(), a, stateport.Key{Fingerprint: "key", Identity: "operator", Kind: "operator"})
			}
		}},
		{"key.scope", func(t *testing.T, s *Store) func(stateport.Action) error {
			key := atomicKey(t, s)
			return func(a stateport.Action) error {
				return s.SetKeyScope(t.Context(), a, key.Fingerprint, []string{"origin"})
			}
		}},
		{"key.revoke", func(t *testing.T, s *Store) func(stateport.Action) error {
			key := atomicKey(t, s)
			return func(a stateport.Action) error { return s.RevokeKey(t.Context(), a, key.Fingerprint) }
		}},
		{"credential.add", func(t *testing.T, s *Store) func(stateport.Action) error {
			c := atomicCredential(t, s)
			return func(a stateport.Action) error { return s.AddCredential(t.Context(), a, c) }
		}},
		{"credential.rotate", func(t *testing.T, s *Store) func(stateport.Action) error {
			c := atomicCredential(t, s)
			requireOK(t, s.AddCredential(t.Context(), stateport.Action{Actor: "operator"}, c))
			next := c
			next.Digest = strings.Repeat("d", 64)
			return func(a stateport.Action) error {
				return s.RotateCredential(t.Context(), a, CredentialRotation{PreviousDigest: c.Digest, Next: next})
			}
		}},
		{"certificate.bind", func(t *testing.T, s *Store) func(stateport.Action) error {
			key := atomicKey(t, s)
			pin := publicPin(t)
			return func(a stateport.Action) error {
				return s.AddCertificatePin(t.Context(), a, stateport.CertificateBinding{Fingerprint: key.Fingerprint, SPKI: pin, ExpiresAt: s.options.Clock().Add(time.Hour)})
			}
		}},
		{"invitation.create", func(t *testing.T, s *Store) func(stateport.Action) error {
			invite, _ := atomicEnrollment(t, s)
			return func(a stateport.Action) error { return s.Invite(t.Context(), a, invite) }
		}},
		{"invitation.reserve", atomicReserve},
		{"invitation.consume", atomicEnroll},
	})
}

func atomicKey(t *testing.T, s *Store) stateport.Key {
	t.Helper()
	key := stateport.Key{Fingerprint: "key", Identity: "operator", Kind: "operator"}
	requireOK(t, s.RegisterKey(t.Context(), stateport.Action{Actor: "operator"}, key))
	return key
}

func atomicCredential(t *testing.T, s *Store) stateport.Credential {
	t.Helper()
	key := atomicKey(t, s)
	return stateport.Credential{Digest: strings.Repeat("c", 64), Fingerprint: key.Fingerprint, ExpiresAt: s.options.Clock().Add(time.Hour)}
}

func atomicEnrollment(t *testing.T, s *Store) (stateport.Invitation, stateport.Enrollment) {
	t.Helper()
	invite := stateport.Invitation{Public: model.Invitation{ID: "invite", Identity: "agent", ServerURL: "https://oberth.ci", CAPin: strings.Repeat("a", 64), Scope: []string{"origin"}, ExpiresAt: s.options.Clock().Add(time.Hour)}, Digest: strings.Repeat("b", 64)}
	input := stateport.Enrollment{InvitationID: invite.Public.ID, Digest: invite.Digest, Reservation: "reservation", SPKI: publicPin(t), Key: stateport.Key{Fingerprint: "agent", Identity: "agent", Kind: "agent", ParkOnPush: true, Scope: []string{"origin"}}, Credential: stateport.Credential{Digest: strings.Repeat("e", 64), Fingerprint: "agent", ExpiresAt: s.options.Clock().Add(time.Hour)}}
	return invite, input
}

func atomicReserve(t *testing.T, s *Store) func(stateport.Action) error {
	t.Helper()
	invite, input := atomicEnrollment(t, s)
	requireOK(t, s.Invite(t.Context(), stateport.Action{Actor: "operator"}, invite))
	return func(a stateport.Action) error {
		_, err := s.ReserveInvitation(t.Context(), stateport.InvitationReservation{ID: input.InvitationID, Digest: input.Digest, Token: input.Reservation, Until: s.options.Clock().Add(time.Minute), Events: a.Events})
		return err
	}
}

func atomicEnroll(t *testing.T, s *Store) func(stateport.Action) error {
	t.Helper()
	reserve := atomicReserve(t, s)
	requireOK(t, reserve(stateport.Action{}))
	_, input := atomicEnrollment(t, s)
	return func(a stateport.Action) error { input.Events = a.Events; return s.Enroll(t.Context(), input) }
}

func TestAtomicConfigurationFacts(t *testing.T) {
	t.Parallel()
	checkAtomicFacts(t, []atomicFact{
		{"secret.grant", func(t *testing.T, s *Store) func(stateport.Action) error {
			r, _ := accepted(t, s, "run")
			return func(a stateport.Action) error {
				return s.SecretGrant(t.Context(), a, workflowpolicy.GrantKey{Key: identityname.Key{Repository: r.Repo, File: "build.yaml", Step: "build"}, Path: "vault/path", Audience: "build"}, false)
			}
		}},
		{"capability.grant", func(t *testing.T, s *Store) func(stateport.Action) error {
			r, _ := accepted(t, s, "run")
			return func(a stateport.Action) error {
				return s.Capability(t.Context(), a, CapabilityGrant{Repository: r.Repo, JSON: []byte(`{"enabled":true}`)})
			}
		}},
		{"claim.grant", func(t *testing.T, s *Store) func(stateport.Action) error {
			r, _ := accepted(t, s, "run")
			return func(a stateport.Action) error {
				return s.GrantClaim(t.Context(), a, ClaimOwner{Claim: "cache", Repository: r.Repo, Tier: "ci"})
			}
		}},
		{"schedule.pause", func(t *testing.T, s *Store) func(stateport.Action) error {
			r, _ := accepted(t, s, "run")
			return func(a stateport.Action) error {
				return s.PauseSchedule(t.Context(), a, SchedulePause{Repository: r.Repo, Entry: "daily", Paused: true})
			}
		}},
		{"action.record", func(t *testing.T, s *Store) func(stateport.Action) error {
			return func(a stateport.Action) error {
				return s.RecordAction(t.Context(), model.AuditAction{Actor: a.Actor, Action: "hold.create", ResourceType: "hold", ResourceID: "operator-hold", Details: `{"reason":"maintenance"}`}, a.Events...)
			}
		}},
	})
}
