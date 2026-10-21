package service

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"testing"

	"github.com/oberthci/oberth/internal/api"
)

func TestAuditListIsAdminOnlyAndReturnsChainPositions(t *testing.T) {
	t.Parallel()
	fixture := newScopeFixture(t)

	for _, actor := range []api.Actor{scopedActor, scopedAdmin} {
		if _, err := fixture.call(actor, "audit_list", map[string]any{}); !errors.Is(err, ErrForbidden) {
			t.Fatalf("scoped %s audit_list error = %v, want forbidden", actor.Identity, err)
		}
	}
	if _, err := fixture.call(api.Actor{Identity: "agent@host", Fingerprint: "SHA256:agent"}, "audit_list", map[string]any{}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("non-admin audit_list error = %v, want forbidden", err)
	}

	value, err := fixture.call(unscopedActor, "audit_list", map[string]any{"limit": 2})
	if err != nil {
		t.Fatal(err)
	}
	page := value.(AuditListResponse)
	if len(page.Entries) != 2 || page.HeadID <= 2 || page.NextBefore != page.Entries[1].ID {
		t.Fatalf("page = %+v", page)
	}
	if page.Entries[0].ID != page.HeadID {
		t.Fatalf("newest entry %d is not the head %d", page.Entries[0].ID, page.HeadID)
	}
	if !page.Entries[0].Verified || page.Entries[0].PreviousSHA256 == "" {
		t.Fatalf("entry = %+v", page.Entries[0])
	}
	older, err := fixture.call(unscopedActor, "audit_list", map[string]any{"before": page.NextBefore, "limit": 1})
	if err != nil {
		t.Fatal(err)
	}
	next := older.(AuditListResponse).Entries[0]
	if next.ID != page.NextBefore-1 || next.SHA256 != page.Entries[1].PreviousSHA256 {
		t.Fatalf("older entry %+v does not precede %+v in the chain", next, page.Entries[1])
	}
	if _, err := hex.DecodeString(next.SHA256); err != nil {
		t.Fatal(err)
	}

	filtered, err := fixture.call(unscopedActor, "audit_list", map[string]any{"action": "repository.", "actor": "owner@tuxbox", "repo": "oberth"})
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range filtered.(AuditListResponse).Entries {
		if len(entry.Action) < 11 || entry.Action[:11] != "repository." {
			t.Fatalf("entry %+v escaped the action filter", entry)
		}
	}
	if _, err := fixture.call(unscopedActor, "audit_list", map[string]any{"since": "yesterday"}); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("bad since error = %v", err)
	}
}

type auditSurfaceAuth struct{ actor api.Actor }

func (auth auditSurfaceAuth) Authenticate(context.Context, string) (api.Actor, error) {
	return auth.actor, nil
}

func TestAuditHTTPMatchesMCPAndRefusesScopedOrNonAdmin(t *testing.T) {
	t.Parallel()
	fixture := newScopeFixture(t)
	for _, actor := range []api.Actor{unscopedActor, scopedActor, scopedAdmin, {Identity: "reader"}} {
		server, err := api.New(auditSurfaceAuth{actor}, fixture.service, fixture.service, "test", api.WithErrorClassifier(func(err error) (int, string) {
			if errors.Is(err, ErrInvalidInput) {
				return http.StatusBadRequest, err.Error()
			}
			return http.StatusInternalServerError, err.Error()
		}))
		if err != nil {
			t.Fatal(err)
		}
		request := httptest.NewRequest(http.MethodGet, "/api/audit?limit=2", nil)
		response := httptest.NewRecorder()
		server.Handler().ServeHTTP(response, request)
		if response.Code != http.StatusUnauthorized {
			t.Fatalf("unauthenticated audit: %d", response.Code)
		}
		request.Header.Set("Authorization", "Bearer test")
		response = httptest.NewRecorder()
		server.Handler().ServeHTTP(response, request)
		if actor.Scoped() || !actor.Admin {
			if response.Code != http.StatusForbidden {
				t.Fatalf("actor %+v: %d %s", actor, response.Code, response.Body)
			}
			continue
		}
		for _, query := range []string{"limit=2", "limit=1&before=3", "action=repository.&actor=owner%40tuxbox&repo=oberth&since=2000-01-01T00:00:00Z&until=2100-01-01T00:00:00Z"} {
			request = httptest.NewRequest(http.MethodGet, "/api/audit?"+query, nil)
			request.Header.Set("Authorization", "Bearer test")
			response = httptest.NewRecorder()
			server.Handler().ServeHTTP(response, request)
			var page AuditListResponse
			if err := json.Unmarshal(response.Body.Bytes(), &page); err != nil || response.Code != http.StatusOK {
				t.Fatalf("HTTP %d %s: %v", response.Code, response.Body, err)
			}
			args := map[string]any{}
			for key, values := range request.URL.Query() {
				if key == "limit" || key == "before" {
					args[key], _ = strconv.Atoi(values[0])
				} else {
					args[key] = values[0]
				}
			}
			value, err := fixture.call(actor, "audit_list", args)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(page, value) {
				t.Fatalf("HTTP=%+v MCP=%+v", page, value)
			}
			if page.NextBefore > 0 && response.Header().Get("X-Oberth-Next-Before") != strconv.FormatInt(page.NextBefore, 10) {
				t.Fatal("missing paging header")
			}
		}
		for _, query := range []string{"limit=bad", "before=-1", "since=yesterday"} {
			request = httptest.NewRequest(http.MethodGet, "/api/audit?"+query, nil)
			request.Header.Set("Authorization", "Bearer test")
			response = httptest.NewRecorder()
			server.Handler().ServeHTTP(response, request)
			if response.Code != http.StatusBadRequest {
				t.Fatalf("%s: %d %s", query, response.Code, response.Body)
			}
		}
	}
}
