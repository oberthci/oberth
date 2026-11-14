package store_test

import (
	"context"
	"crypto/sha256"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/storetest"
	"github.com/oberthci/oberth/internal/api"
	"github.com/oberthci/oberth/internal/auth"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/service"
	"github.com/oberthci/oberth/internal/store"
)

type failedTokenReads struct {
	*store.Store
	err error
}

func (r failedTokenReads) AuthenticatedUplinkByDigest(context.Context, []byte) (model.AuthenticatedUplink, error) {
	return model.AuthenticatedUplink{}, r.err
}

func TestAuthenticationReadFailureIsUnavailableForValidToken(t *testing.T) {
	db, err := storetest.Open(t.Context(), filepath.Join(t.TempDir(), "auth.sqlite"), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	plaintext := "oberth_" + strings.Repeat("A", 43)
	digest := sha256.Sum256([]byte(plaintext))
	credential, err := db.CreateTokenCredential(t.Context(), model.TokenCredentialSpec{Name: "valid", Digest: digest[:]})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := storetest.UpsertUplink(db, t.Context(), model.UplinkSpec{Identity: "reader", Fingerprint: "SHA256:reader", TokenCredentialID: credential.ID, AuthActor: "test"}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ActivateTokenCredential(t.Context(), credential.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.AuthenticatedUplinkByDigest(t.Context(), digest[:]); err != nil {
		t.Fatalf("fixture token is not valid: %v", err)
	}
	for _, tc := range []struct {
		name   string
		err    error
		status int
	}{
		{"read-error", errors.New("SQLITE_IOERR private-canary"), http.StatusServiceUnavailable},
		{"unknown-or-revoked", store.ErrNotFound, http.StatusUnauthorized},
	} {
		t.Run(tc.name, func(t *testing.T) {
			authenticator, err := service.NewAuthenticator(auth.Authenticator{Tokens: failedTokenReads{Store: db, err: tc.err}})
			if err != nil {
				t.Fatal(err)
			}
			views := &repositoryViews{db: db}
			server, err := api.New(authenticator, views, views, "test")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(server.BeginDrain)
			request := httptest.NewRequest(http.MethodGet, "/api/repos", nil)
			request.Header.Set("Authorization", "Bearer "+plaintext)
			response := httptest.NewRecorder()
			server.Handler().ServeHTTP(response, request)
			if response.Code != tc.status || views.calls != 0 || strings.Contains(response.Body.String(), "private-canary") {
				t.Fatalf("read failure misclassified or reached view: status=%d calls=%d body=%s", response.Code, views.calls, response.Body)
			}
			if tc.status == http.StatusServiceUnavailable && (response.Header().Get("Retry-After") != "1" || response.Header().Get("WWW-Authenticate") != "" || !strings.Contains(response.Body.String(), "temporarily unavailable")) {
				t.Fatalf("read error claimed invalid token: headers=%v body=%s", response.Header(), response.Body)
			}
		})
	}
}
