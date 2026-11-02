package client

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

type headerGetter interface {
	GetWithHeaders(context.Context, string, map[string]string, any) (http.Header, error)
}

func TestGetPreservesPaginationHeaders(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Oberth-Next-Before", "opaque-cursor")
		_, _ = w.Write([]byte(`[]`))
	}))
	defer server.Close()
	getter, ok := any(newClient(t, server)).(headerGetter)
	if !ok {
		t.Fatal("client drops pagination headers")
	}
	var rows []any
	headers, err := getter.GetWithHeaders(t.Context(), "/api/runs", nil, &rows)
	if err != nil || headers.Get("X-Oberth-Next-Before") != "opaque-cursor" {
		t.Fatalf("headers=%v err=%v", headers, err)
	}
}
