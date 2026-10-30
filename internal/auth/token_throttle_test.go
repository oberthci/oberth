package auth

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/store"
)

type countingTouchRepository struct {
	*store.Store
	touches, lookups int
}

func (r *countingTouchRepository) TouchTokenCredential(ctx context.Context, id string) error {
	r.touches++
	return r.Store.TouchTokenCredential(ctx, id)
}
func (r *countingTouchRepository) AuthenticatedUplinkByDigest(ctx context.Context, digest []byte) (model.AuthenticatedUplink, error) {
	r.lookups++
	return r.Store.AuthenticatedUplinkByDigest(ctx, digest)
}

func TestTokenUseThrottleSkipsWriterButReadsRevocationEveryTime(t *testing.T) {
	now := time.Now().UTC()
	path := filepath.Join(t.TempDir(), "token.sqlite")
	db, err := store.Open(t.Context(), path, store.Options{Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	issuer := Issuer{Tokens: db, Bind: func(ctx context.Context, pending model.TokenCredential) error {
		_, err := db.UpsertUplink(ctx, model.UplinkSpec{Identity: "reader", Fingerprint: "SHA256:" + pending.ID, TokenCredentialID: pending.ID, AuthActor: "test"})
		return err
	}}
	var out bytes.Buffer
	token, err := issuer.Issue(t.Context(), "reader", &out)
	if err != nil {
		t.Fatal(err)
	}
	plaintext := strings.TrimSpace(out.String())
	repository := &countingTouchRepository{Store: db}
	auth := Authenticator{Tokens: repository, Now: func() time.Time { return now }}
	if _, err := auth.Authenticate(t.Context(), plaintext); err != nil {
		t.Fatal(err)
	}
	if repository.touches != 1 {
		t.Fatal("first use was not recorded")
	}
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	tx, err := raw.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(t.Context(), "UPDATE token_credentials SET name=name WHERE id=?", token.ID); err != nil {
		t.Fatal(err)
	}
	// A fresh token identity snapshot still succeeds while a different writer
	// holds the DB. Neither a hot token nor a token at59s needs the writer.
	for range 4 {
		ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
		_, err := auth.Authenticate(ctx, plaintext)
		cancel()
		if err != nil {
			t.Fatal(err)
		}
	}
	now = now.Add(59 * time.Second)
	if _, err := auth.Authenticate(t.Context(), plaintext); err != nil {
		t.Fatal(err)
	}
	if repository.touches != 1 || repository.lookups != 6 {
		t.Fatalf("throttle cached authority or touched: %+v", repository)
	}
	now = now.Add(time.Second)
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	_, err = auth.Authenticate(ctx, plaintext)
	cancel()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("due touch ignored writer/deadline: %v", err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	if _, err := auth.Authenticate(t.Context(), plaintext); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256([]byte(plaintext))
	readback, err := db.TokenCredentialByDigest(t.Context(), digest[:])
	if err != nil || readback.LastUsedAt == nil || !readback.LastUsedAt.Equal(now) {
		t.Fatalf("due use not recorded: %+v %v", readback, err)
	}
	if err := db.RevokeTokenCredential(t.Context(), token.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := auth.Authenticate(t.Context(), plaintext); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("recent use bypassed revocation: %v", err)
	}
	if repository.lookups != 9 {
		t.Fatalf("authentication skipped fresh lookups: %d", repository.lookups)
	}
}
