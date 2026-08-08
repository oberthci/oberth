package auth

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/store"
)

type shortWriter struct{ bytes.Buffer }

func (writer *shortWriter) Write(body []byte) (int, error) {
	_, _ = writer.Buffer.Write(body)
	return len(body) - 1, nil
}

type activationFailRepository struct{ *store.Store }

func (repository activationFailRepository) ActivateTokenCredential(context.Context, string) (model.TokenCredential, error) {
	return model.TokenCredential{}, errors.New("activation unavailable")
}

type countingWriter struct {
	bytes.Buffer
	writes int
}

func (w *countingWriter) Write(body []byte) (int, error) {
	w.writes++
	return w.Buffer.Write(body)
}

func TestIssueAndAuthenticateTokenWithoutPlaintextPersistence(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "oberth.db")
	now := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	database, err := store.Open(ctx, path, store.Options{Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}

	issuer := Issuer{Tokens: database, Bind: func(ctx context.Context, pending model.TokenCredential) error {
		_, err := database.UpsertUplink(ctx, model.UplinkSpec{
			Fingerprint: "SHA256:key", Identity: "agent@host", TokenCredentialID: pending.ID, AuthActor: "bootstrap",
		})
		return err
	}}
	var output countingWriter
	credential, err := issuer.Issue(ctx, "automation", &output)
	if err != nil {
		t.Fatal(err)
	}
	if output.writes != 1 {
		t.Fatalf("token output writes = %d, want exactly one", output.writes)
	}
	plaintext := string(bytes.TrimSpace(output.Bytes()))
	if plaintext == "" {
		t.Fatal("issued token is empty")
	}
	wantDigest := sha256.Sum256([]byte(plaintext))
	if !bytes.Equal(credential.Digest, wantDigest[:]) {
		t.Fatalf("stored digest = %x, want %x", credential.Digest, wantDigest)
	}

	if credential.ActivatedAt == nil {
		t.Fatal("issued credential is inactive")
	}
	authenticator := Authenticator{Tokens: database}
	authenticated, err := authenticator.Authenticate(ctx, plaintext)
	if err != nil || authenticated.TokenCredential.ID != credential.ID || authenticated.Identity != "agent@host" || authenticated.Fingerprint != "SHA256:key" {
		t.Fatalf("authenticate = %#v, %v", authenticated, err)
	}
	if _, err := authenticator.Authenticate(ctx, plaintext+"wrong"); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("invalid token error = %v, want ErrInvalidToken", err)
	}

	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(body, []byte(plaintext)) {
		t.Fatal("plaintext token found in SQLite database")
	}
}

func TestIssueLeavesCredentialInactiveWhenOutputOrActivationFails(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	bind := func(database *store.Store, fingerprint string) func(context.Context, model.TokenCredential) error {
		return func(ctx context.Context, pending model.TokenCredential) error {
			_, err := database.UpsertUplink(ctx, model.UplinkSpec{
				Fingerprint: fingerprint, Identity: fingerprint + "@host", TokenCredentialID: pending.ID, AuthActor: "bootstrap",
			})
			return err
		}
	}
	for name, issue := range map[string]func(*store.Store) ([]byte, error){
		"short output": func(database *store.Store) ([]byte, error) {
			var output shortWriter
			_, err := (Issuer{Tokens: database, Bind: bind(database, "SHA256:short")}).Issue(ctx, "short", &output)
			return bytes.TrimSpace(output.Bytes()), err
		},
		"binding failure": func(database *store.Store) ([]byte, error) {
			var output bytes.Buffer
			_, err := (Issuer{Tokens: database, Bind: func(context.Context, model.TokenCredential) error {
				return errors.New("binding unavailable")
			}}).Issue(ctx, "binding", &output)
			return bytes.TrimSpace(output.Bytes()), err
		},
		"activation failure": func(database *store.Store) ([]byte, error) {
			var output bytes.Buffer
			_, err := (Issuer{Tokens: activationFailRepository{Store: database}, Bind: bind(database, "SHA256:activation")}).Issue(ctx, "activation", &output)
			return bytes.TrimSpace(output.Bytes()), err
		},
	} {
		t.Run(name, func(t *testing.T) {
			database, err := store.Open(ctx, filepath.Join(t.TempDir(), "oberth.db"), store.Options{Now: func() time.Time { return now }})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = database.Close() })
			plaintext, issueErr := issue(database)
			if issueErr == nil {
				t.Fatal("Issue unexpectedly succeeded")
			}
			if name == "short output" && !errors.Is(issueErr, io.ErrShortWrite) {
				t.Fatalf("Issue error = %v, want io.ErrShortWrite", issueErr)
			}
			digest := sha256.Sum256(plaintext)
			credential, err := database.TokenCredentialByDigest(ctx, digest[:])
			if err != nil {
				t.Fatal(err)
			}
			if credential.ActivatedAt != nil {
				t.Fatalf("credential activated after failed issuance: %#v", credential)
			}
			if credential.RevokedAt == nil {
				t.Fatalf("failed issuance lacks rollback-safe revocation marker: %#v", credential)
			}
		})
	}
}

func TestUnboundPendingCredentialCannotActivate(t *testing.T) {
	ctx := context.Background()
	database, err := store.Open(ctx, filepath.Join(t.TempDir(), "oberth.db"), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	digest := sha256.Sum256([]byte("pending"))
	credential, err := database.CreateTokenCredential(ctx, model.TokenCredentialSpec{Name: "pending", Digest: digest[:]})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.ActivateTokenCredential(ctx, credential.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("unbound activation error = %v, want not found", err)
	}
}

func TestIssuedTokensAreRandom(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	database, err := store.Open(ctx, filepath.Join(t.TempDir(), "oberth.db"), store.Options{Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	issuer := Issuer{Tokens: database, Bind: func(ctx context.Context, pending model.TokenCredential) error {
		_, err := database.UpsertUplink(ctx, model.UplinkSpec{
			Fingerprint: "SHA256:" + pending.ID, Identity: pending.Name + "@host", TokenCredentialID: pending.ID, AuthActor: "bootstrap",
		})
		return err
	}}
	var first, second bytes.Buffer
	if _, err := issuer.Issue(ctx, "first", &first); err != nil {
		t.Fatal(err)
	}
	if _, err := issuer.Issue(ctx, "second", &second); err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(first.Bytes(), second.Bytes()) {
		t.Fatal("two issued tokens were identical")
	}
}
