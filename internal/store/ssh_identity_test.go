package store

import (
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/publicerr"
	"github.com/oberthci/oberth/internal/stateport"
)

func TestSSHKeyUsesUplinkExpiryAndCertificateRenewal(t *testing.T) {
	options := testOptions()
	var clock atomic.Int64
	clock.Store(options.Clock().UnixNano())
	options.Clock = func() time.Time { return time.Unix(0, clock.Load()) }
	s, err := Open(t.Context(), baselineDatabase.Path(t), options)
	requireOK(t, err)
	t.Cleanup(func() { requireOK(t, s.Close()) })
	now := options.Clock()
	expiry := now.Add(time.Hour)
	action := stateport.Action{Actor: "operator"}
	key := stateport.Key{Fingerprint: "uplink", Identity: "agent", Kind: "agent", ParkOnPush: true, ExpiresAt: expiry}
	requireOK(t, s.RegisterKey(t.Context(), action, key))
	requireOK(t, s.AddCredential(t.Context(), action, stateport.Credential{Digest: strings.Repeat("a", 64), Fingerprint: key.Fingerprint, ExpiresAt: expiry.Add(time.Hour)}))
	_, err = s.SSHKey(t.Context(), key.Fingerprint)
	requireOK(t, err)
	now = expiry
	clock.Store(now.UnixNano())
	_, err = s.SSHKey(t.Context(), key.Fingerprint)
	requireCode(t, err, publicerr.Forbidden)
	_, err = s.Authenticate(t.Context(), 0, strings.Repeat("a", 64))
	requireCode(t, err, publicerr.Forbidden)
	requireOK(t, s.AddCertificatePin(t.Context(), action, stateport.CertificateBinding{Fingerprint: key.Fingerprint, SPKI: publicPin(t), ExpiresAt: now.Add(time.Hour)}))
	renewed, err := s.SSHKey(t.Context(), key.Fingerprint)
	requireOK(t, err)
	if !renewed.ExpiresAt.Equal(now.Add(time.Hour)) {
		t.Fatal("renewal did not persist the uplink lifetime")
	}
	requireOK(t, s.RevokeKey(t.Context(), action, key.Fingerprint))
	revoked, err := s.SSHKey(t.Context(), key.Fingerprint)
	requireOK(t, err)
	if !revoked.Revoked {
		t.Fatal("SSH lookup lost revocation for the authority boundary")
	}

}
