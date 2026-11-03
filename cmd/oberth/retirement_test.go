package main

import (
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type retirementDeadlineRecorder struct {
	*httptest.ResponseRecorder
	deadline time.Time
}

func (w *retirementDeadlineRecorder) SetWriteDeadline(deadline time.Time) error {
	w.deadline = deadline
	return nil
}

func TestRetirementDeadlineIsRouteLocal(t *testing.T) {
	const ordinary = 5 * time.Second
	server := withLegacyRemovalCheck(&http.Server{Handler: http.NotFoundHandler(), WriteTimeout: ordinary}, "/fixture.sqlite", func(context.Context) error { return nil })
	if server.WriteTimeout != ordinary {
		t.Fatalf("shared write timeout widened to %v", server.WriteTimeout)
	}
	for _, path := range []string{"/", "/v1/retirement-check"} {
		out := &retirementDeadlineRecorder{ResponseRecorder: httptest.NewRecorder()}
		request := httptest.NewRequest(http.MethodGet, path, nil)
		request.Header.Set("X-Oberth-Admin-Database", "/fixture.sqlite")
		start := time.Now()
		server.Handler.ServeHTTP(out, request)
		if path == "/" {
			if !out.deadline.IsZero() {
				t.Fatal("ordinary route deadline changed")
			}
		} else if out.deadline.Before(start.Add(3*time.Minute)) || out.deadline.After(time.Now().Add(3*time.Minute)) {
			t.Fatalf("retirement deadline is not bounded to three minutes: %v", out.deadline)
		}
	}
}

// Lifecycle boundary: the live inventory is allowed only in the on-demand
// admin handler, never as a synchronous prerequisite to starting listeners.
func TestServeDoesNotExecuteRetirementInventory(t *testing.T) {
	source, err := parser.ParseFile(token.NewFileSet(), "serve.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	ast.Inspect(source, func(node ast.Node) bool {
		if _, deferred := node.(*ast.FuncLit); deferred {
			return false
		}
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		method, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		owner, ok := method.X.(*ast.Ident)
		if ok && owner.Name == "removalCheck" {
			t.Errorf("startup executes retirement inventory through %s", method.Sel.Name)
		}
		return true
	})
}

func TestRetirementFaultsLeaveListenersAvailable(t *testing.T) {
	for _, fault := range []string{"upstream unavailable", "repository archived", "new legacy branch"} {
		t.Run(fault, func(t *testing.T) {
			calls := 0
			server := withLegacyRemovalCheck(&http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusOK)
			})}, "/fixture.sqlite", func(context.Context) error { calls++; return errors.New(fault) })
			if calls != 0 {
				t.Fatal("creating server executed inventory")
			}
			for _, check := range []bool{false, true, false} {
				path, want := "/", http.StatusOK
				if check {
					path, want = "/v1/retirement-check", http.StatusConflict
				}
				request := httptest.NewRequest(http.MethodGet, path, nil)
				request.Header.Set("X-Oberth-Admin-Database", "/fixture.sqlite")
				out := httptest.NewRecorder()
				server.Handler.ServeHTTP(out, request)
				if out.Code != want {
					t.Fatalf("%s returned %d, want %d", path, out.Code, want)
				}
			}
			if calls != 1 {
				t.Fatalf("inventory called %d times", calls)
			}
		})
	}
}

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
