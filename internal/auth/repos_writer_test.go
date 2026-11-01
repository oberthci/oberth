package auth_test

import (
	"bytes"
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/api"
	"github.com/oberthci/oberth/internal/auth"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/service"
	"github.com/oberthci/oberth/internal/store"
	"github.com/oberthci/oberth/internal/testtimeout"
)

type repositoryViews struct {
	api.ViewService
	api.ToolService
	api.ArtifactDownloads
	db    *store.Store
	calls int
}

func (v *repositoryViews) Repositories(ctx context.Context, _ api.Actor) (any, error) {
	v.calls++
	return v.db.ListRepositories(ctx)
}

// Use the real HTTP authentication chain and a real uncommitted WAL writer.
// Only the unused view/tool methods are stubbed. The repository read itself
// must be reached even when the token's last-used timestamp needs updating.
func TestReposWithDueTokenUseWhileWriterHeld(t *testing.T) {
	for _, usedBefore := range []bool{false, true} {
		t.Run(map[bool]string{false: "first-use", true: "expired-touch"}[usedBefore], func(t *testing.T) {
			now := time.Now().UTC()
			path := filepath.Join(t.TempDir(), "repos.sqlite")
			db, err := store.Open(t.Context(), path, store.Options{Now: func() time.Time { return now }})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = db.Close() })
			if err := db.OpenSSHAuthReaders(t.Context()); err != nil {
				t.Fatal(err)
			}
			var output bytes.Buffer
			credential, err := (auth.Issuer{Tokens: db, Bind: func(ctx context.Context, pending model.TokenCredential) error {
				_, err := db.UpsertUplink(ctx, model.UplinkSpec{Identity: "reader", Fingerprint: "SHA256:reader", TokenCredentialID: pending.ID, AuthActor: "test"})
				return err
			}}).Issue(t.Context(), "reader", &output)
			if err != nil {
				t.Fatal(err)
			}
			if usedBefore {
				if err := db.TouchTokenCredential(t.Context(), credential.ID); err != nil {
					t.Fatal(err)
				}
				now = now.Add(store.TokenCredentialTouchInterval)
			}
			authenticator, err := service.NewAuthenticator(auth.Authenticator{Tokens: db, Now: func() time.Time { return now }})
			if err != nil {
				t.Fatal(err)
			}
			views := &repositoryViews{db: db}
			server, err := api.New(authenticator, views, views, "test")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(server.BeginDrain)
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
			if _, err := tx.ExecContext(t.Context(), "UPDATE token_credentials SET name=name WHERE id=?", credential.ID); err != nil {
				t.Fatal(err)
			}
			request := httptest.NewRequest(http.MethodGet, "/api/repos", nil)
			request.Header.Set("Authorization", "Bearer "+strings.TrimSpace(output.String()))
			response := httptest.NewRecorder()
			started := time.Now()
			server.Handler().ServeHTTP(response, request)
			elapsed := time.Since(started)
			t.Logf("held-writer /api/repos: status=%d elapsed=%s body=%s", response.Code, elapsed, response.Body)
			if response.Code != http.StatusOK || views.calls != 1 || elapsed >= testtimeout.Start(time.Second) {
				t.Fatalf("due token telemetry blocked repository view: status=%d view calls=%d elapsed=%s", response.Code, views.calls, elapsed)
			}
			if err := tx.Rollback(); err != nil {
				t.Fatal(err)
			}
			// Once the writer is available, usage must eventually be recorded.
			deadline := time.Now().Add(testtimeout.Start(2 * time.Second))
			for {
				got, err := db.TokenCredentialByDigest(t.Context(), credential.Digest)
				if err != nil {
					t.Fatal(err)
				}
				if got.LastUsedAt != nil && got.LastUsedAt.Equal(now) {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("deferred token use was not recorded after writer release")
				}
				time.Sleep(time.Millisecond)
			}
		})
	}
}
