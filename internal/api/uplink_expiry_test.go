package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestDashboardUplinkExpiryRequiresUnscopedAdmin(t *testing.T) {
	for _, actor := range []Actor{{Identity: "owner", Admin: true}, {Identity: "worker"}, {Identity: "scoped", Admin: true, Scope: []string{"github"}}} {
		server, backend := testServer(t)
		server.auth = streamAuthFunc(func(context.Context, string) (Actor, error) { return actor, nil })
		backend.toolResult = map[string]any{"uplinks": []map[string]any{{"identity": "legacy", "no_expiry": true}}}
		request := httptest.NewRequest(http.MethodGet, "/api/uplinks", nil)
		request.Header.Set("Authorization", "Bearer valid-token")
		response := httptest.NewRecorder()
		server.Handler().ServeHTTP(response, request)
		wanted := http.StatusForbidden
		if actor.Admin && !actor.Scoped() {
			wanted = http.StatusOK
		}
		if response.Code != wanted {
			t.Errorf("actor %+v: %d, want %d", actor, response.Code, wanted)
		}
		if wanted == http.StatusOK && backend.calledName != "uplink_list" {
			t.Errorf("dashboard did not use authorized uplink listing: %q", backend.calledName)
		}
		if wanted == http.StatusForbidden && backend.calledName != "" {
			t.Fatal("unauthorized request reached uplink listing")
		}
	}
}
