package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestIssueDetailRequiresAuthenticationAndUsesScopedIssueGet(t *testing.T) {
	server, backend := testServer(t)
	backend.toolResult = map[string]any{"id": 42, "body": "full issue body"}
	for _, tc := range []struct {
		path, token string
		status      int
	}{
		{"/api/issues/42", "", http.StatusUnauthorized},
		{"/api/issues/42", "valid-token", http.StatusOK},
		{"/api/issues/nope", "valid-token", http.StatusBadRequest},
		{"/api/issues/0", "valid-token", http.StatusBadRequest},
	} {
		request := httptest.NewRequest(http.MethodGet, tc.path, nil)
		if tc.token != "" {
			request.Header.Set("Authorization", "Bearer "+tc.token)
		}
		response := httptest.NewRecorder()
		server.Handler().ServeHTTP(response, request)
		if response.Code != tc.status {
			t.Fatalf("%s: %d: %s", tc.path, response.Code, response.Body.String())
		}
		if tc.status == http.StatusOK {
			var args map[string]any
			if err := json.Unmarshal(backend.calledArguments, &args); err != nil {
				t.Fatal(err)
			}
			if backend.calledName != "issue_get" || args["id"] != float64(42) || args["body_limit"] != nil || backend.calledActor.Identity != "agent@host" || !strings.Contains(response.Body.String(), "full issue body") {
				t.Fatalf("detail bypassed issue_get: %+v %s", backend, response.Body.String())
			}
		}
	}
}
