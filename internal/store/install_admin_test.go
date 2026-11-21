package store

import (
	"context"
	"crypto/ed25519"
	"crypto/x509"
	"encoding/base64"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/publicerr"
	"github.com/oberthci/oberth/internal/stateport"
)

func installRegistration(t *testing.T, n byte) stateport.IdentityRegistration {
	t.Helper()
	seed := make([]byte, ed25519.SeedSize)
	seed[0] = n
	private := ed25519.NewKeyFromSeed(seed)
	der, err := x509.MarshalPKIXPublicKey(private.Public())
	requireOK(t, err)
	return stateport.IdentityRegistration{Key: stateport.Key{Fingerprint: fmt.Sprintf("install-%d", n), Identity: "install-admin", Kind: "installer", Admin: true}, SPKI: base64.StdEncoding.EncodeToString(der)}
}
func TestInstallAdminRollsBackFactsAuditAndEvents(t *testing.T) {
	t.Parallel()
	checkAtomicFacts(t, []atomicFact{{"identity.install", func(t *testing.T, s *Store) func(stateport.Action) error {
		requireOK(t, s.InstallAdmin(t.Context(), stateport.Action{Actor: "installation"}, installRegistration(t, 'a')))
		return func(a stateport.Action) error { return s.InstallAdmin(t.Context(), a, installRegistration(t, 'b')) }
	}}})
}
func TestInstallAdminPersistsReplacementAndOtherAdministrators(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	first, next := installRegistration(t, 'a'), installRegistration(t, 'b')
	other := installRegistration(t, 'c')
	other.Key.Identity, other.Key.Kind = "another-admin", "operator"
	a := stateport.Action{Actor: "installation"}
	requireOK(t, s.RegisterIdentity(t.Context(), a, other))
	requireOK(t, s.InstallAdmin(t.Context(), a, first))
	credential := stateport.Credential{Fingerprint: first.Key.Fingerprint, Digest: strings.Repeat("d", 64), ExpiresAt: s.options.Clock().Add(time.Hour)}
	requireOK(t, s.AddCredential(t.Context(), a, credential))
	requireOK(t, s.InstallAdmin(t.Context(), a, next))
	path := s.path
	requireOK(t, s.Close())
	reopened, err := Open(t.Context(), path, testOptions())
	requireOK(t, err)
	t.Cleanup(func() { requireOK(t, reopened.Close()) })
	_, err = reopened.AuthenticateCertificate(t.Context(), 0, first.SPKI)
	requireCode(t, err, publicerr.Forbidden)
	oldKey, err := reopened.Key(t.Context(), first.Key.Fingerprint)
	requireOK(t, err)
	if !oldKey.Revoked {
		t.Fatal("old push authority survived reopen")
	}
	_, err = reopened.Authenticate(t.Context(), 0, credential.Digest)
	requireCode(t, err, publicerr.Forbidden)
	for _, v := range []stateport.IdentityRegistration{next, other} {
		actor, err := reopened.AuthenticateCertificate(t.Context(), 0, v.SPKI)
		requireOK(t, err)
		if !actor.Admin || actor.Fingerprint != v.Key.Fingerprint {
			t.Fatal("replacement affected administrator authority", actor)
		}
	}
}
func TestInstallAdminRefusesExistingKeyAndPinConflicts(t *testing.T) {
	t.Parallel()
	for _, which := range []string{"fingerprint", "pin"} {
		t.Run(which, func(t *testing.T) {
			s := testStore(t)
			first, next := installRegistration(t, 'a'), installRegistration(t, 'b')
			a := stateport.Action{Actor: "installation"}
			requireOK(t, s.InstallAdmin(t.Context(), a, first))
			occupied := next
			occupied.Key.Identity, occupied.Key.Kind = "existing-user", "operator"
			occupied.Key.Admin = false
			occupied.Key.Scope = []string{"origin"}
			if which == "pin" {
				occupied.Key.Fingerprint = "existing-fingerprint"
			} else {
				occupied.SPKI = installRegistration(t, 'c').SPKI
			}
			requireOK(t, s.RegisterIdentity(t.Context(), a, occupied))
			before := databaseContents(t, s.readers)
			requireCode(t, s.InstallAdmin(t.Context(), a, next), publicerr.Conflict)
			if databaseContents(t, s.readers) != before {
				t.Fatal("key or pin conflict changed install authority")
			}
		})
	}
}
func TestInstallAdminRefusesNonInstallAuthority(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	a := stateport.Action{Actor: "installation"}
	for _, change := range []func(*stateport.IdentityRegistration){
		func(v *stateport.IdentityRegistration) { v.Key.Admin = false },
		func(v *stateport.IdentityRegistration) { v.Key.Kind = "operator" },
		func(v *stateport.IdentityRegistration) { v.Key.Scope = []string{"origin"} },
		func(v *stateport.IdentityRegistration) { v.Key.ParkOnPush = true },
		func(v *stateport.IdentityRegistration) { v.Key.Revoked = true },
		func(v *stateport.IdentityRegistration) { v.SPKI = "" },
		func(v *stateport.IdentityRegistration) {
			v.Credential = &stateport.Credential{Fingerprint: v.Key.Fingerprint, Digest: strings.Repeat("d", 64), ExpiresAt: s.options.Clock().Add(time.Hour)}
		},
	} {
		v := installRegistration(t, 'a')
		change(&v)
		before := databaseContents(t, s.readers)
		requireCode(t, s.InstallAdmin(t.Context(), a, v), publicerr.InvalidInput)
		if databaseContents(t, s.readers) != before {
			t.Fatal("invalid install binding changed authority")
		}
	}
}

var _ interface {
	InstallAdmin(context.Context, stateport.Action, stateport.IdentityRegistration) error
} = (*Store)(nil)
