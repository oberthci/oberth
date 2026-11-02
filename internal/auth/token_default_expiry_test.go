package auth

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/store"
)

// New issuance now has the owner-approved default lifetime. This is the
// original unbounded-lifetime reproduction, retained as an executable regression.
func TestTokenRejectsCenturyOldCredential(t *testing.T) {
	now := time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)
	database, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "oberth.sqlite"), store.Options{
		Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	issuer := Issuer{Tokens: database, Bind: func(ctx context.Context, credential model.TokenCredential) error {
		_, err := database.UpsertUplink(ctx, model.UplinkSpec{
			Identity: "expiry@host", Fingerprint: "SHA256:expiry", TokenCredentialID: credential.ID, AuthActor: "test",
		})
		return err
	}}
	var output bytes.Buffer
	if _, err := issuer.Issue(t.Context(), "expiry", &output); err != nil {
		t.Fatal(err)
	}
	authenticator := Authenticator{Tokens: database, Now: func() time.Time { return now.AddDate(100, 0, 0) }}
	if _, err := authenticator.Authenticate(t.Context(), strings.TrimSpace(output.String())); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("century-old credential authentication error = %v, want ErrInvalidToken", err)
	}
}
