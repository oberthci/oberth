package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestIssuesCLIReadsAllPages(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Query().Get("before") == "1" {
			_, _ = w.Write([]byte(`{"Issues":[{"ID":1,"Title":"older"}],"total":2}`))
		} else {
			_, _ = w.Write([]byte(`{"Issues":[{"ID":2,"Title":"newer"}],"NextBefore":1,"total":2}`))
		}
	}))
	defer server.Close()
	configure(t, server)
	var out bytes.Buffer
	if err := runIssues(t.Context(), nil, &out); err != nil {
		t.Fatal(err)
	}
	if requests != 2 || !strings.Contains(out.String(), "older") {
		t.Fatalf("requests=%d output=%s", requests, out.String())
	}
}

func TestIssuesJSONPreservesIntegerPrecision(t *testing.T) {
	configure(t, remoteServer(t, `{"Issues":[{"ID":9007199254740993,"Title":"exact"}],"total":1}`))
	var out bytes.Buffer
	if err := runIssues(t.Context(), []string{"--json"}, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "9007199254740993") {
		t.Fatalf("issue ID lost precision: %s", out.String())
	}
}
