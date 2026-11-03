package service

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/api"
	"github.com/oberthci/oberth/internal/auth"
	"github.com/oberthci/oberth/internal/model"
)

func TestExpiredTokenReturnsUnauthorizedAndExpiryIsListed(t *testing.T) {
	f := newScopeFixture(t)
	f.service.uplinkLifecycle = f.store
	expires := time.Now().Add(time.Hour).UTC()
	var output bytes.Buffer
	_, err := (auth.Issuer{Tokens: f.store, ExpiresAt: &expires, Bind: func(ctx context.Context, pending model.TokenCredential) error {
		_, err := f.store.RegisterUplink(ctx, "operator", model.UplinkSpec{Identity: "expiry@host", Fingerprint: "SHA256:expiry", TokenCredentialID: pending.ID})
		return err
	}}).Issue(t.Context(), "expiry", &output)
	if err != nil {
		t.Fatal(err)
	}
	authenticator, err := NewAuthenticator(auth.Authenticator{Tokens: f.store, Now: func() time.Time { return expires }})
	if err != nil {
		t.Fatal(err)
	}
	server, err := api.New(authenticator, f.service, f.service, "test")
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodGet, "/api/repos", nil)
	request.Header.Set("Authorization", "Bearer "+strings.TrimSpace(output.String()))
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("expired token: %d", response.Code)
	}
	value, err := f.call(unscopedActor, "uplink_list", map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	views := value.(map[string]any)["uplinks"].([]uplinkView)
	if len(views) != 1 || views[0].ExpiresAt == nil || !views[0].ExpiresAt.Equal(expires) {
		t.Fatalf("expiry absent from uplink_list: %+v", views)
	}
}
