package auth

import (
	"context"
	"crypto/sha256"
	"errors"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/store"
)

type shapeLookupRepository struct {
	TokenRepository
	lookups int
	digest  []byte
}

func (r *shapeLookupRepository) AuthenticatedUplinkByDigest(_ context.Context, digest []byte) (model.AuthenticatedUplink, error) {
	r.lookups++
	r.digest = append([]byte(nil), digest...)
	return model.AuthenticatedUplink{}, store.ErrNotFound
}

func TestBearerShapeRejectsJunkBeforeRepositoryAccess(t *testing.T) {
	repository := &shapeLookupRepository{}
	auth := Authenticator{Tokens: repository}
	valid := "oberth_" + strings.Repeat("A", 43)
	for _, token := range []string{"", "junk", valid + "A", valid[:49], "wrong__" + strings.Repeat("A", 43), "oberth_" + strings.Repeat(" ", 43), valid[:49] + "=", valid[:49] + "/", valid[:49] + "+", valid[:49] + "\n", valid[:49] + "\x00", valid[:49] + "\xff"} {
		if _, err := auth.Authenticate(t.Context(), token); !errors.Is(err, ErrInvalidToken) {
			t.Fatalf("malformed bearer accepted, length=%d: %v", len(token), err)
		}
	}
	if repository.lookups != 0 {
		t.Fatalf("junk input reached the database: %d lookups", repository.lookups)
	}
	// A well-shaped unknown token must still be freshly checked, not authorized
	// from its syntax. Use both URL-base64 punctuation characters as well.
	for _, token := range []string{valid, "oberth_" + strings.Repeat("a-9_", 10) + "XYZ"} {
		if _, err := auth.Authenticate(t.Context(), token); !errors.Is(err, ErrInvalidToken) {
			t.Fatalf("unknown token accepted: %v", err)
		}
		expected := sha256.Sum256([]byte(token))
		if string(repository.digest) != string(expected[:]) {
			t.Fatal("well-shaped token did not use its full digest")
		}
	}
	if repository.lookups != 2 {
		t.Fatalf("well-shaped unknown token skipped fresh checks: %d", repository.lookups)
	}
}
