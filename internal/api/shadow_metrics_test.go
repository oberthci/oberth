package api

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestShadowMetricsRequireUnscopedAdministrator(t *testing.T) {
	for _, tc := range []struct {
		name  string
		actor Actor
		want  int
	}{
		{"reader", Actor{Identity: "reader"}, http.StatusForbidden},
		{"scoped admin", Actor{Identity: "scoped", Admin: true, Scope: []string{"github"}}, http.StatusForbidden},
		{"admin", Actor{Identity: "admin", Admin: true}, http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server, _ := testServer(t)
			server.auth = streamAuthFunc(func(context.Context, string) (Actor, error) { return tc.actor, nil })
			WithMetrics(func(w io.Writer) { _, _ = fmt.Fprint(w, "oberth_gitcache_shadow_mismatch_total 7\n") })(server)
			request := httptest.NewRequest(http.MethodGet, "/metrics", nil)
			request.Header.Set("Authorization", "Bearer valid-token")
			response := httptest.NewRecorder()
			server.Handler().ServeHTTP(response, request)
			if response.Code != tc.want {
				t.Fatalf("status=%d body=%s", response.Code, response.Body)
			}
			if (strings.Contains(response.Body.String(), "oberth_gitcache_shadow_mismatch_total 7")) != (tc.want == http.StatusOK) {
				t.Fatal(response.Body.String())
			}
		})
	}
	server, _ := testServer(t)
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if response.Code != http.StatusUnauthorized {
		t.Fatal(response.Code)
	}
}
