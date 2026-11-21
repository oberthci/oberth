package api_test

import (
	"net/http/httptest"
	"strings"
	"testing"
)

type unreadBody struct{ t *testing.T }

func (b unreadBody) Read([]byte) (int, error) { b.t.Fatal("unauthorized body was read"); return 0, nil }
func (unreadBody) Close() error               { return nil }

func TestSecretClassIsCheckedBeforeBody(t *testing.T) {
	f := setup(t, nil)
	c := admin()
	c.Actor.Admin = false
	f.auth.Set(c, nil)
	r := httptest.NewRequest("POST", "/api/secrets/put", nil)
	r.Body = unreadBody{t}
	r.Header.Set("Authorization", "Bearer valid")
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	f.server.Handler().ServeHTTP(w, r)
	if w.Code != 403 || !strings.Contains(w.Body.String(), "Use an identity authorized") {
		t.Fatal(w.Code, w.Body.String())
	}
}

func TestStreamsAndMetricsDisableContentSniffing(t *testing.T) {
	f := setup(t, nil)
	for _, path := range []string{"/metrics", "/api/runs/12345678/containerlog/stream?node_id=node&container=main"} {
		r := httptest.NewRequest("GET", path, nil)
		r.Header.Set("Authorization", "Bearer valid")
		w := newStreamWriter()
		f.server.Handler().ServeHTTP(w, r)
		if w.Code != 200 || w.Header().Get("X-Content-Type-Options") != "nosniff" {
			t.Fatal(path, w.Code, w.Header(), w.Body.String())
		}
	}
}
