package store

import (
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/stateport"
)

func TestKeyRevocationTimeIsStableAndDurable(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	at := s.options.Clock()
	now := at
	s.options.Clock = func() time.Time { return now }
	k := stateport.Key{Fingerprint: "agent", Identity: "agent", Kind: "agent", ParkOnPush: true}
	requireOK(t, s.RegisterKey(t.Context(), stateport.Action{Actor: "operator"}, k))
	requireOK(t, s.RevokeKey(t.Context(), stateport.Action{Actor: "operator"}, k.Fingerprint))
	now = at.Add(time.Hour)
	requireOK(t, s.RevokeKey(t.Context(), stateport.Action{Actor: "operator"}, k.Fingerprint))
	key, err := s.Key(t.Context(), k.Fingerprint)
	requireOK(t, err)
	if key.RevokedAt == nil || !key.RevokedAt.Equal(at) {
		t.Fatal("revocation time changed", key)
	}
	keys, err := s.IdentityKeys(t.Context(), k.Identity)
	requireOK(t, err)
	if len(keys) != 1 || keys[0].RevokedAt == nil || !keys[0].RevokedAt.Equal(at) {
		t.Fatal("key list lost revocation time", keys)
	}
	path, options := s.path, s.options
	requireOK(t, s.Close())
	reopened, err := Open(t.Context(), path, options)
	requireOK(t, err)
	t.Cleanup(func() { requireOK(t, reopened.Close()) })
	key, err = reopened.Key(t.Context(), k.Fingerprint)
	requireOK(t, err)
	if key.RevokedAt == nil || !key.RevokedAt.Equal(at) {
		t.Fatal("revocation time did not survive reopen", key)
	}
}

func TestCertificateOnlyEnrollmentCreatesNoBearer(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	reserve := atomicReserve(t, s)
	requireOK(t, reserve(stateport.Action{}))
	_, input := atomicEnrollment(t, s)
	input.Credential = stateport.Credential{}
	requireOK(t, s.Enroll(t.Context(), input))
	var count int
	requireOK(t, s.readers.QueryRowContext(t.Context(), `SELECT count(*) FROM credentials`).Scan(&count))
	if count != 0 {
		t.Fatal("certificate-only enrollment created a bearer", count)
	}
	who, err := s.AuthenticateCertificate(t.Context(), 0, input.SPKI)
	requireOK(t, err)
	if who.Identity != input.Key.Identity {
		t.Fatal("certificate was not bound", who)
	}
}
