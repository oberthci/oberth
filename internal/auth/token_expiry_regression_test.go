package auth

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/store"
"github.com/oberthci/oberth/internal/storetest"
)

// An explicit expiry is durable and takes effect at the exact boundary.
func TestIssuedTokenExpiry(t *testing.T) {
	now := time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)
	database, err := storetest.Open(t.Context(), filepath.Join(t.TempDir(), "oberth.sqlite"), store.Options{
		Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	expires := now.Add(time.Hour)
	issuer := Issuer{Tokens: database, ExpiresAt: &expires, Bind: func(ctx context.Context, credential model.TokenCredential) error {
		_, err := storetest.UpsertUplink(database, ctx, model.UplinkSpec{
			Identity: "expiry@host", Fingerprint: "SHA256:expiry", TokenCredentialID: credential.ID, AuthActor: "test",
		})
		return err
	}}
	var output bytes.Buffer
	if _, err := issuer.Issue(t.Context(), "expiry", &output); err != nil {
		t.Fatal(err)
	}
	plaintext := strings.TrimSpace(output.String())
	authenticator := Authenticator{Tokens: database, Now: func() time.Time { return expires.Add(-time.Nanosecond) }}
	if _, err := authenticator.Authenticate(t.Context(), plaintext); err != nil {
		t.Fatalf("unexpired token rejected: %v", err)
	}
	for _, at := range []time.Time{expires, expires.Add(time.Second)} {
		authenticator.Now = func() time.Time { return at }
		if _, err := authenticator.Authenticate(t.Context(), plaintext); !errors.Is(err, ErrInvalidToken) {
			t.Fatalf("expired credential authentication error = %v, want ErrInvalidToken", err)
		}
	}
	digest := sha256.Sum256([]byte(plaintext))
	stored, err := storetest.TokenCredentialByDigest(database, t.Context(), digest[:])
	if err != nil || stored.ExpiresAt == nil || !stored.ExpiresAt.Equal(expires) {
		t.Fatalf("expiry round trip: %+v, %v", stored.ExpiresAt, err)
	}
	statuses, err := database.ListUplinkStatuses(t.Context())
	if err != nil || len(statuses) != 1 || statuses[0].ExpiresAt == nil || !statuses[0].ExpiresAt.Equal(expires) {
		t.Fatalf("expiry listing: %+v, %v", statuses, err)
	}
}
