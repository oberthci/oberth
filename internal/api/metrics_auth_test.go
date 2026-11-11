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

func TestMetricsRequireUnscopedAdministrator(t *testing.T) {
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
			WithMetrics(func(w io.Writer) { _, _ = fmt.Fprint(w, "oberth_fixture_total 7\n") })(server)
			request := httptest.NewRequest(http.MethodGet, "/metrics", nil)
			request.Header.Set("Authorization", "Bearer valid-token")
			response := httptest.NewRecorder()
			server.Handler().ServeHTTP(response, request)
			if response.Code != tc.want {
				t.Fatalf("status=%d body=%s", response.Code, response.Body)
			}
			if (strings.Contains(response.Body.String(), "oberth_fixture_total 7")) != (tc.want == http.StatusOK) {
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

// A metrics-scoped token reads /metrics and is refused on every other
// authenticated route (#1534).
func TestMetricsScopedTokenReadsOnlyMetrics(t *testing.T) {
	metrics := Actor{Identity: "metrics@prometheus", Fingerprint: "SHA256:metrics", Scope: []string{"metrics"}}
	server, _ := testServer(t)
	server.auth = streamAuthFunc(func(context.Context, string) (Actor, error) { return metrics, nil })
	WithMetrics(func(w io.Writer) { _, _ = fmt.Fprint(w, "oberth_fixture_total 7\n") })(server)
	serve := func(method, path string) *httptest.ResponseRecorder {
		request := httptest.NewRequest(method, path, strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
		request.Header.Set("Authorization", "Bearer valid-token")
		request.Header.Set("Content-Type", "application/json")
		response := httptest.NewRecorder()
		server.Handler().ServeHTTP(response, request)
		return response
	}
	if response := serve(http.MethodGet, "/metrics"); response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "oberth_fixture_total 7") {
		t.Fatalf("metrics: status=%d body=%s", response.Code, response.Body)
	}
	for _, route := range []string{
		"POST /api/admission/hold", "POST /api/promotions", "POST /mcp", "GET /mcp", "DELETE /mcp",
		"GET /api/runs", "GET /api/runs/counts", "GET /api/run-resources", "GET /api/runs/r1/resources",
		"GET /api/runs/r1", "GET /api/runs/r1/logs", "GET /api/runs/r1/livelog/stream", "GET /api/runs/r1/livelog",
		"GET /api/runs/r1/artifacts", "GET /api/runs/r1/artifacts/a.txt", "GET /api/repos", "GET /api/issues/1",
		"GET /api/issues", "GET /api/grants", "GET /api/audit", "GET /api/schedules", "GET /api/status", "GET /api/uplinks",
	} {
		method, path, _ := strings.Cut(route, " ")
		if response := serve(method, path); response.Code != http.StatusForbidden || !strings.Contains(response.Body.String(), "only GET /metrics") {
			t.Errorf("%s: status=%d body=%s", route, response.Code, response.Body)
		}
	}
	// Combined with admin it still reads only /metrics.
	server.auth = streamAuthFunc(func(context.Context, string) (Actor, error) {
		return Actor{Identity: metrics.Identity, Fingerprint: metrics.Fingerprint, Admin: true, Scope: metrics.Scope}, nil
	})
	if response := serve(http.MethodGet, "/api/uplinks"); response.Code != http.StatusForbidden {
		t.Fatalf("metrics admin uplinks: %d", response.Code)
	}
}
