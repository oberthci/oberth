package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRESTPromotionUsesAuthenticatedTool(t *testing.T) {
	backend := &fakeBackend{toolResult: PromoteResponse{ID: "train-id"}}
	server, err := New(backend, backend, backend, "test")
	if err != nil {
		t.Fatal(err)
	}
	body := `{"repo":"codeberg.org/acme/widgets","branch":"main","sources":[{"sha":"1111111111111111111111111111111111111111"},{"sha":"2222222222222222222222222222222222222222"}],"skip_conflicting":true}`
	for _, auth := range []bool{false, true} {
		req := httptest.NewRequest(http.MethodPost, "/api/promotions", strings.NewReader(body))
		if auth {
			req.Header.Set("Authorization", "Bearer valid-token")
		}
		rec := httptest.NewRecorder()
		server.Handler().ServeHTTP(rec, req)
		if !auth {
			if rec.Code != http.StatusUnauthorized || backend.calledName != "" {
				t.Fatalf("unauthenticated: %d %s", rec.Code, rec.Body)
			}
			continue
		}
		if rec.Code != http.StatusOK || backend.calledName != "promote" || string(backend.calledArguments) != body || backend.calledActor.Identity != "agent@host" {
			t.Fatalf("REST dispatch: %d %s %+v", rec.Code, rec.Body, backend)
		}
	}
	backend.calledName = ""
	req := httptest.NewRequest(http.MethodPost, "/api/promotions", strings.NewReader(strings.Repeat(" ", 1<<20+1)))
	req.Header.Set("Authorization", "Bearer valid-token")
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusRequestEntityTooLarge || backend.calledName != "" {
		t.Fatalf("oversized request: %d", rec.Code)
	}
}
