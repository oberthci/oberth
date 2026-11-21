package service_test

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/api"
	apifake "github.com/oberthci/oberth/internal/api/fake"
	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/publicerr"
	"github.com/oberthci/oberth/internal/stepmodel"
)

func TestScheduledRunIDHTTPUsesRealServiceAndStoreScope(t *testing.T) {
	t.Parallel()
	f, r := scheduledRunFixture(t, repository)
	auth := apifake.NewAuth(apicontract.Caller{Actor: admin(), CredentialID: "test-credential"})
	server := serviceHTTPServer(t, f, auth)
	for _, tc := range []struct {
		name, id, scope string
		status          int
	}{
		{"visible", r.ID, "origin", http.StatusOK},
		{"hidden", r.ID, "private", http.StatusNotFound},
		{"timestamp_prefix", r.ID[:len(r.ID)-1], "origin", http.StatusNotFound},
		{"malformed", r.ID + "-extra", "origin", http.StatusBadRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := apicontract.Caller{Actor: admin(), CredentialID: "test-credential"}
			c.Actor.Scope = []string{tc.scope}
			f.store.Actors[c.Actor.Fingerprint] = c.Actor
			auth.Set(c, nil)
			request := httptest.NewRequest(http.MethodGet, "/api/runs/"+tc.id, nil)
			request.Header.Set("Authorization", "Bearer valid")
			response := httptest.NewRecorder()
			server.Handler().ServeHTTP(response, request)
			if response.Code != tc.status {
				t.Fatalf("HTTP %d, want %d: %s", response.Code, tc.status, response.Body.String())
			}
			if tc.status == http.StatusOK {
				var got stepmodel.RunSummary
				if err := json.Unmarshal(response.Body.Bytes(), &got); err != nil {
					t.Fatal(err)
				}
				if got.ID != r.ID || got.Repo != r.Repo {
					t.Fatal("HTTP lost scheduled identity", got)
				}
			}
		})
	}
}

func serviceHTTPServer(t *testing.T, f *fixture, auth *apifake.Authenticator) *api.Server {
	t.Helper()
	registry, err := api.Builtin(f.s.Bindings())
	if err != nil {
		t.Fatal(err)
	}
	runtime := apifake.NewClock().Runtime()
	runtime.Now = time.Now //nolint:forbidigo // Real HTTP socket deadlines follow wall time.
	server, err := api.New(api.Config{Auth: auth, Registry: registry, Runtime: runtime, Classify: publicerr.Classify, FailureLog: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(server.BeginDrain)
	return server
}

func TestScheduledRunCursorHTTPContinuesAndKeepsRepositoryScope(t *testing.T) {
	t.Parallel()
	f, ids := scheduledPageFixture(t)
	c := apicontract.Caller{Actor: admin(), CredentialID: "test-credential"}
	auth := apifake.NewAuth(c)
	server := serviceHTTPServer(t, f, auth)
	first := scheduledHTTPPage(t, server, "")
	assertScheduledPage(t, first, ids[:2], ids[1])
	second := scheduledHTTPPage(t, server, first.NextCursor)
	assertScheduledPage(t, second, ids[2:], "")
	c.Actor.Scope = []string{"private"}
	f.store.Actors[c.Actor.Fingerprint] = c.Actor
	auth.Set(c, nil)
	request := httptest.NewRequest(http.MethodGet, "/api/runs?limit=2&before="+url.QueryEscape(first.NextCursor), nil)
	request.Header.Set("Authorization", "Bearer valid")
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusNotFound {
		t.Fatalf("hidden scheduled cursor: %d %s", response.Code, response.Body.String())
	}
}

func scheduledHTTPPage(t *testing.T, server *api.Server, cursor string) apicontract.RunPage {
	t.Helper()
	request := httptest.NewRequest(http.MethodGet, "/api/runs?limit=2&before="+url.QueryEscape(cursor), nil)
	request.Header.Set("Authorization", "Bearer valid")
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("scheduled HTTP page: %d %s", response.Code, response.Body.String())
	}
	var page apicontract.RunPage
	if err := json.Unmarshal(response.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	return page
}
