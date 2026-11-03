package auth

import (
	"context"
	"crypto/sha256"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"
)

// Return an inconsistent repository snapshot to exercise the authenticator's
// own checks independently of the SQL repository's filtering.
type credentialGuardRepository struct {
	TokenRepository
	value model.AuthenticatedUplink
	uses  int
	err   error
}

func (r *credentialGuardRepository) AuthenticatedUplinkByDigest(context.Context, []byte) (model.AuthenticatedUplink, error) {
	return r.value, r.err
}

func (r *credentialGuardRepository) RecordTokenCredentialUse(context.Context, string) { r.uses++ }

func assertCredentialRefused(t *testing.T, corrupt func(*model.TokenCredential)) {
	t.Helper()
	plaintext := "oberth_" + strings.Repeat("A", 43)
	digest := sha256.Sum256([]byte(plaintext))
	now := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	repository := &credentialGuardRepository{value: model.AuthenticatedUplink{
		Uplink:          model.Uplink{Identity: "operator", Fingerprint: "SHA256:fixture", Admin: true},
		TokenCredential: model.TokenCredential{ID: "credential", Digest: digest[:]},
	}}
	authenticator := Authenticator{Tokens: repository, Now: func() time.Time { return now }}
	if got, err := authenticator.Authenticate(t.Context(), plaintext); err != nil || got.Identity != "operator" {
		t.Fatalf("valid snapshot refused: %+v, %v", got, err)
	}
	repository.uses = 0
	corrupt(&repository.value.TokenCredential)
	got, err := authenticator.Authenticate(t.Context(), plaintext)
	if !errors.Is(err, ErrInvalidToken) || !reflect.DeepEqual(got, model.AuthenticatedUplink{}) || repository.uses != 0 {
		t.Fatalf("invalid snapshot authorized or recorded as used: %+v, %v, uses=%d", got, err, repository.uses)
	}
}

func TestAuthenticateRefusesRevokedSnapshot(t *testing.T) {
	assertCredentialRefused(t, func(credential *model.TokenCredential) {
		revoked := time.Date(2026, 9, 30, 0, 0, 0, 0, time.UTC)
		credential.RevokedAt = &revoked
	})
}

func TestAuthenticateRefusesDifferentDigest(t *testing.T) {
	assertCredentialRefused(t, func(credential *model.TokenCredential) {
		digest := sha256.Sum256([]byte("another fixture credential"))
		credential.Digest = digest[:]
	})
}

func TestAuthenticateFailsClosedOnRepositoryError(t *testing.T) {
	plaintext := "oberth_" + strings.Repeat("A", 43)
	digest := sha256.Sum256([]byte(plaintext))
	failure := errors.New("identity snapshot unavailable")
	repository := &credentialGuardRepository{err: failure, value: model.AuthenticatedUplink{
		Uplink:          model.Uplink{Identity: "operator", Fingerprint: "SHA256:fixture", Admin: true},
		TokenCredential: model.TokenCredential{ID: "credential", Digest: digest[:]},
	}}
	got, err := (Authenticator{Tokens: repository}).Authenticate(t.Context(), plaintext)
	if !errors.Is(err, failure) || !reflect.DeepEqual(got, model.AuthenticatedUplink{}) || repository.uses != 0 {
		t.Fatalf("failed identity lookup authorized or recorded as used: %+v, %v, uses=%d", got, err, repository.uses)
	}
}
