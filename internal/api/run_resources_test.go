package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/measured"
)

func TestRunResourcesRouteAuthenticatesAndUsesSharedTool(t *testing.T) {
	s, b := testServer(t)
	b.toolResult = measured.Report{Repository: "repo-01", Steps: []measured.Step{}}
	for _, path := range []string{"/api/run-resources?repo=repo-01&last=7&include_failed=true&resource_only=true", "/api/runs/aaaaaaaa/resources?format=yaml"} {
		r := httptest.NewRequest(http.MethodGet, path, nil)
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, r)
		if w.Code != 401 {
			t.Fatal("unauthenticated report", w.Code)
		}
		r = httptest.NewRequest(http.MethodGet, path, nil)
		r.Header.Set("Authorization", "Bearer valid-token")
		w = httptest.NewRecorder()
		s.Handler().ServeHTTP(w, r)
		if w.Code != 200 || b.calledName != "run_resources" || b.calledActor.Identity == "" {
			t.Fatalf("route: %d %s", w.Code, w.Body)
		}
		if strings.Contains(path, "yaml") && !strings.Contains(w.Header().Get("Content-Type"), "yaml") {
			t.Fatal("yaml content type")
		}
		var a map[string]any
		if e := json.Unmarshal(b.calledArguments, &a); e != nil {
			t.Fatal(e)
		}
		if strings.Contains(path, "last=7") && (a["last"] != float64(7) || a["include_failed"] != true) {
			t.Fatal(a)
		}
		if a["resource_only"] != strings.Contains(path, "resource_only=true") {
			t.Fatalf("resource-only capability not forwarded: %v", a)
		}
	}
	for _, query := range []string{"last=no", "include_failed=maybe", "resource_only=maybe"} {
		r := httptest.NewRequest(http.MethodGet, "/api/run-resources?"+query, nil)
		r.Header.Set("Authorization", "Bearer valid-token")
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, r)
		if w.Code != 400 {
			t.Fatal(w.Code)
		}
	}
}
