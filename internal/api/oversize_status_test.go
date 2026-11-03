package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestOversizedBodiesReturn413(t *testing.T) {
	s, b := testServer(t)
	for _, path := range []string{"/mcp"} {
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{"text":"`+strings.Repeat("x", 5<<20)+`"}`))
		req.Header.Set("Authorization", "Bearer valid-token")
		out := httptest.NewRecorder()
		s.Handler().ServeHTTP(out, req)
		if out.Code != http.StatusRequestEntityTooLarge || b.calledName != "" {
			t.Errorf("%s: %d %s", path, out.Code, out.Body)
		}
	}
}
