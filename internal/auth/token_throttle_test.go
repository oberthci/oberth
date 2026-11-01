package auth

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/store"
	"github.com/oberthci/oberth/internal/testtimeout"
)

type countingTouchRepository struct {
	*store.Store
	touches, lookups int
}

func (r *countingTouchRepository) RecordTokenCredentialUse(ctx context.Context, id string) {
	r.touches++
	r.Store.RecordTokenCredentialUse(ctx, id)
}
func (r *countingTouchRepository) AuthenticatedUplinkByDigest(ctx context.Context, digest []byte) (model.AuthenticatedUplink, error) {
	r.lookups++
	return r.Store.AuthenticatedUplinkByDigest(ctx, digest)
}

func TestTokenUseThrottleSkipsWriterButReadsRevocationEveryTime(t *testing.T) {
	now := time.Now().UTC()
	var clock atomic.Int64
	clock.Store(now.UnixNano())
	readClock := func() time.Time { return time.Unix(0, clock.Load()).UTC() }
	path := filepath.Join(t.TempDir(), "token.sqlite")
	db, err := store.Open(t.Context(), path, store.Options{Now: readClock})
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
	digest := sha256.Sum256([]byte(plaintext))
	waitForUse := func() {
		t.Helper()
		deadline := time.Now().Add(testtimeout.Start(2 * time.Second))
		for {
			readback, err := db.TokenCredentialByDigest(t.Context(), digest[:])
			if err != nil {
				t.Fatal(err)
			}
			if readback.LastUsedAt != nil && readback.LastUsedAt.Equal(now) {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("due use not recorded: %+v", readback)
			}
			time.Sleep(time.Millisecond)
		}
	}
	repository := &countingTouchRepository{Store: db}
	auth := Authenticator{Tokens: repository, Now: readClock}
	if _, err := auth.Authenticate(t.Context(), plaintext); err != nil {
		t.Fatal(err)
	}
	if repository.touches != 1 {
		t.Fatal("first use was not queued")
	}
	waitForUse()
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
	// holds the DB. Neither a hot token nor a token at 59s queues an update.
	for range 4 {
		ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
		_, err := auth.Authenticate(ctx, plaintext)
		cancel()
		if err != nil {
			t.Fatal(err)
		}
	}
	now = now.Add(59 * time.Second)
	clock.Store(now.UnixNano())
	if _, err := auth.Authenticate(t.Context(), plaintext); err != nil {
		t.Fatal(err)
	}
	if repository.touches != 1 || repository.lookups != 6 {
		t.Fatalf("throttle cached authority or touched: %+v", repository)
	}
	now = now.Add(time.Second)
	clock.Store(now.UnixNano())
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	_, err = auth.Authenticate(ctx, plaintext)
	cancel()
	if err != nil || repository.touches != 2 {
		t.Fatalf("due telemetry blocked authentication or was not queued: %v touches=%d", err, repository.touches)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	if _, err := auth.Authenticate(t.Context(), plaintext); err != nil {
		t.Fatal(err)
	}
	waitForUse()
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
