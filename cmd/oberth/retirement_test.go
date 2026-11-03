package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestDaemonRetirementCheckIsReadOnlyAndNamesBlockingRepository(t *testing.T) {
	for _, blocked := range []bool{true, false} {
		calls := 0
		server := withLegacyRemovalCheck(&http.Server{Handler: http.NotFoundHandler()}, "/fixture.sqlite", func(context.Context) error {
			calls++
			if blocked {
				return errors.New("repository forge/team/kernel file .oberth/tests.yaml still declares legacy execution")
			}
			return nil
		})
		request := httptest.NewRequest(http.MethodGet, "/v1/retirement-check", nil)
		request.Header.Set("X-Oberth-Admin-Database", "/fixture.sqlite")
		out := httptest.NewRecorder()
		server.Handler.ServeHTTP(out, request)
		if calls != 1 {
			t.Fatal("daemon check was bypassed")
		}
		if blocked {
			if out.Code != http.StatusConflict || !strings.Contains(out.Body.String(), "forge/team/kernel") {
				t.Fatalf("%d %s", out.Code, out.Body)
			}
		} else if out.Code != http.StatusNoContent || out.Header().Get("X-Oberth-Retirement-Version") != "1" {
			t.Fatalf("%d %s", out.Code, out.Body)
		}
		request.Header.Set("X-Oberth-Admin-Database", "/other.sqlite")
		out = httptest.NewRecorder()
		server.Handler.ServeHTTP(out, request)
		if calls != 1 || out.Code != http.StatusConflict {
			t.Fatal("other database accepted")
		}
	}
}
